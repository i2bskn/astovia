package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/chatwork"
	"github.com/i2bskn/astovia/internal/chatworksync"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/githubsync"
	"github.com/i2bskn/astovia/internal/gmailsync"
	"github.com/i2bskn/astovia/internal/google"
	"github.com/i2bskn/astovia/internal/httpserver"
	"github.com/i2bskn/astovia/internal/notify"
	"github.com/i2bskn/astovia/internal/secretbox"
	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/slacksync"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/internal/worker"
	"github.com/i2bskn/astovia/web"
	"golang.org/x/term"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run(args []string) error {
	users := len(args) > 0 && args[0] == "users"
	if !users && len(args) > 0 && (len(args) != 1 || args[0] != "serve") {
		return errors.New("usage: astovia [serve | users create/reset-password/disable/enable --email ADDRESS [--password-stdin] [--role owner|member]]")
	}
	c, e := config.Load(!users)
	if e != nil {
		return e
	}
	ctx := context.Background()
	s, e := store.Open(ctx, c.Driver, c.DSN)
	if e != nil {
		return e
	}
	defer func() { _ = s.Close() }()
	if e = s.CheckSchema(ctx); e != nil {
		return e
	}
	if users {
		return account(ctx, s, args[1:], os.Stdin)
	}
	assets, e := web.Assets()
	if e != nil {
		return errors.New("embedded frontend is unavailable; build web first")
	}
	a, e := auth.New(s, c.Secret, c.PublicURL)
	if e != nil {
		return errors.New("cannot initialize authentication")
	}
	srv := &http.Server{Addr: c.Addr, Handler: httpserver.New(c, a, assets), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	signals, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	background, cancelBackground := context.WithCancel(ctx)
	defer cancelBackground()
	workers := startWorkers(background, s, c, a)
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Printf("Astovia listening on %s", c.Addr)
	select {
	case e = <-done:
		cancelBackground()
		waitWorkers(workers, time.Now().Add(10*time.Second))
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-signals.Done():
		shutdown, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		// The loops stop together with the HTTP server; the run in
		// progress may take until the same deadline.
		cancelBackground()
		e = srv.Shutdown(shutdown)
		deadline, _ := shutdown.Deadline()
		waitWorkers(workers, deadline)
		return e
	}
}

// startWorkers starts the background loops of serve: GitHub synchronization
// (with the AI judging mentions), Slack, Chatwork and mail synchronization
// (with the AI judging messages and replies; Chatwork also imports tasks;
// mail does nothing without the Google OAuth client), the notification
// engine, the dispatcher, the cleanup of sent notifications and the sweep
// of task candidates and passed events. They share the
// authentication service's clock. They stop when ctx is canceled; the returned group
// finishes when they have returned.
func startWorkers(ctx context.Context, s *store.Store, c config.Config, a *auth.Service) *sync.WaitGroup {
	now := func() time.Time { return a.Now() }
	box := secretbox.New(c.Secret)
	aiClient := ai.New()
	syncer := &githubsync.Syncer{Store: s, GitHub: github.New(now), AI: aiClient, Box: box, Now: now}
	slackSyncer := &slacksync.Syncer{Store: s, Slack: slack.New(now), AI: aiClient, Box: box, Now: now}
	chatworkSyncer := &chatworksync.Syncer{Store: s, Chatwork: chatwork.New(now), AI: aiClient, Box: box, Now: now}
	gmailSyncer := &gmailsync.Syncer{Store: s, Google: google.New(now), Client: c.Google, AI: aiClient, Box: box, Now: now}
	engine := &notify.Engine{Store: s, Now: now}
	sweeper := &notify.Sweeper{Store: s, Now: now}
	vapid := &notify.VAPID{Store: s, Box: box, Now: now}
	dispatcher := &notify.Dispatcher{Store: s, Channel: notify.NewWebPush(s, vapid, c.PublicURL, now), Now: now}
	var wg sync.WaitGroup
	for _, l := range []struct {
		name     string
		interval time.Duration
		fn       func(context.Context)
	}{
		{"github sync", 15 * time.Second, syncer.RunOnce},
		{"slack sync", 15 * time.Second, slackSyncer.RunOnce},
		{"chatwork sync", 15 * time.Second, chatworkSyncer.RunOnce},
		{"gmail sync", 15 * time.Second, gmailSyncer.RunOnce},
		{"notification engine", time.Minute, engine.RunOnce},
		{"notification dispatcher", 5 * time.Second, dispatcher.RunOnce},
		{"notification cleanup", time.Hour, dispatcher.Cleanup},
		{"candidate sweep", 5 * time.Minute, sweeper.RunOnce},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker.Loop(ctx, l.name, l.interval, l.fn)
		}()
	}
	return &wg
}

// waitWorkers waits for the loops to return, at most until deadline.
func waitWorkers(wg *sync.WaitGroup, deadline time.Time) {
	finished := make(chan struct{})
	go func() {
		wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Until(deadline)):
		log.Print("background work did not stop in time")
	}
}
func account(ctx context.Context, s *store.Store, args []string, in io.Reader) error {
	if len(args) < 1 {
		return errors.New("users command requires create, reset-password, disable, or enable")
	}
	op := args[0]
	if op != "create" && op != "reset-password" && op != "disable" && op != "enable" {
		return errors.New("unknown users command")
	}
	f := flag.NewFlagSet(op, flag.ContinueOnError)
	email := f.String("email", "", "email address")
	stdin := f.Bool("password-stdin", false, "read password from stdin")
	role := f.String("role", store.RoleOwner, "role of the new user (owner or member)")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	roleSet := false
	f.Visit(func(fl *flag.Flag) { roleSet = roleSet || fl.Name == "role" })
	if op != "create" && roleSet {
		return errors.New(op + " does not accept --role")
	}
	if !store.ValidRole(*role) {
		return errors.New("role must be owner or member")
	}
	u, e := auth.Email(*email)
	if e != nil {
		return e
	}
	hash := ""
	if op == "disable" || op == "enable" {
		if *stdin {
			return errors.New(op + " does not accept --password-stdin")
		}
	} else {
		p, e := readPassword(in, *stdin)
		if e != nil {
			return e
		}
		hash, e = auth.Password(p)
		if e != nil {
			return e
		}
	}
	switch op {
	case "create":
		id, e := auth.Random()
		if e != nil {
			return e
		}
		e = s.CreateUser(ctx, store.User{ID: id, Email: u, PasswordHash: hash, CreatedAt: time.Now().Unix(), Role: *role})
		if e != nil {
			if errors.Is(e, store.ErrConflict) {
				return e
			}
			return errors.New("could not create user")
		}
	case "enable":
		e = s.EnableUser(ctx, u)
		if e != nil {
			if errors.Is(e, store.ErrNotFound) {
				return e
			}
			return errors.New("could not update user")
		}
	default:
		// The CLI does not apply the last-owner rule of the user management
		// API: it is the operator's last resort.
		e = s.ChangeUser(ctx, u, hash, op == "disable", time.Now().Unix())
		if e != nil {
			if errors.Is(e, store.ErrNotFound) {
				return e
			}
			return errors.New("could not update user")
		}
	}
	fmt.Println("User operation completed.")
	return nil
}

// maxPasswordInput is the longest accepted --password-stdin input, including
// one trailing line break.
const maxPasswordInput = 75

// readPassword reads the password from in when fromStdin is set. The input
// must be a single line: one trailing \r\n, \n or \r is removed, and any other
// line break or input longer than maxPasswordInput bytes is rejected.
// Otherwise it prompts on the terminal attached to os.Stdin.
func readPassword(in io.Reader, fromStdin bool) (string, error) {
	if fromStdin {
		b, e := io.ReadAll(io.LimitReader(in, maxPasswordInput+1))
		if e != nil {
			return "", errors.New("could not read password")
		}
		if len(b) > maxPasswordInput {
			return "", errors.New("password input is too long")
		}
		p := string(b)
		switch {
		case strings.HasSuffix(p, "\r\n"):
			p = strings.TrimSuffix(p, "\r\n")
		case strings.HasSuffix(p, "\n"):
			p = strings.TrimSuffix(p, "\n")
		case strings.HasSuffix(p, "\r"):
			p = strings.TrimSuffix(p, "\r")
		}
		if strings.ContainsAny(p, "\r\n") {
			return "", errors.New("password must be a single line")
		}
		return p, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("use --password-stdin for noninteractive input")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	b, e := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if e != nil {
		return "", errors.New("could not read password")
	}
	return string(b), nil
}
