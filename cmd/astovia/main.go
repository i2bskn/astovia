package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/i2bskn/astovia/internal/auth"
	"github.com/i2bskn/astovia/internal/config"
	"github.com/i2bskn/astovia/internal/httpserver"
	"github.com/i2bskn/astovia/internal/store"
	"github.com/i2bskn/astovia/web"
	"golang.org/x/term"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
		return errors.New("usage: astovia [serve | users create/reset-password/disable --email ADDRESS [--password-stdin]]")
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
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	log.Printf("Astovia listening on %s", c.Addr)
	select {
	case e = <-done:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-signals.Done():
		shutdown, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
func account(ctx context.Context, s *store.Store, args []string, in io.Reader) error {
	if len(args) < 1 {
		return errors.New("users command requires create, reset-password, or disable")
	}
	op := args[0]
	if op != "create" && op != "reset-password" && op != "disable" {
		return errors.New("unknown users command")
	}
	f := flag.NewFlagSet(op, flag.ContinueOnError)
	email := f.String("email", "", "email address")
	stdin := f.Bool("password-stdin", false, "read password from stdin")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	u, e := auth.Email(*email)
	if e != nil {
		return e
	}
	hash := ""
	if op != "disable" {
		p, e := readPassword(in, *stdin)
		if e != nil {
			return e
		}
		hash, e = auth.Password(p)
		if e != nil {
			return e
		}
	} else if *stdin {
		return errors.New("disable does not accept --password-stdin")
	}
	if op == "create" {
		id, e := auth.Random()
		if e != nil {
			return e
		}
		e = s.CreateUser(ctx, store.User{ID: id, Email: u, PasswordHash: hash, CreatedAt: time.Now().Unix()})
		if e != nil {
			if errors.Is(e, store.ErrConflict) {
				return e
			}
			return errors.New("could not create user")
		}
	} else {
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
