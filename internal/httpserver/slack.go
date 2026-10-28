package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/slack"
	"github.com/i2bskn/astovia/internal/slacksync"
	"github.com/i2bskn/astovia/internal/store"
)

// slackTimeout bounds the token check against Slack.
const slackTimeout = 10 * time.Second

// slackIDPattern matches Slack workspace and member IDs.
var slackIDPattern = regexp.MustCompile(`^[A-Z0-9]{1,32}$`)

// slackResponse is a connected Slack integration. The token is never
// included; token_last4 identifies it.
type slackResponse struct {
	Connected       bool     `json:"connected"`
	TeamName        string   `json:"team_name"`
	TeamURL         string   `json:"team_url"`
	UserName        string   `json:"user_name"`
	TokenLast4      string   `json:"token_last4"`
	Scopes          []string `json:"scopes"`
	ImmediateNotify bool     `json:"immediate_notify"`
	DetectMentions  bool     `json:"detect_mentions"`
	DetectDMs       bool     `json:"detect_dms"`
	DetectGroupDMs  bool     `json:"detect_group_dms"`
	Status          string   `json:"status"`
	LastError       *string  `json:"last_error"`
	LastSyncedAt    *string  `json:"last_synced_at"`
	CreatedAt       string   `json:"created_at"`
}

func toSlackResponse(c store.SlackConnection) slackResponse {
	out := slackResponse{Connected: true, TeamName: c.TeamName, TeamURL: c.TeamURL, UserName: c.SlackUserName, TokenLast4: c.TokenLast4, Scopes: slack.ParseScopes(c.Scopes),
		ImmediateNotify: c.ImmediateNotify == 1, DetectMentions: c.DetectMentions == 1, DetectDMs: c.DetectDMs == 1, DetectGroupDMs: c.DetectGroupDMs == 1, Status: c.Status, CreatedAt: formatTime(c.CreatedAt)}
	if c.LastError.Valid {
		out.LastError = &c.LastError.String
	}
	if c.LastSyncedAt.Valid {
		v := formatTime(c.LastSyncedAt.Int64)
		out.LastSyncedAt = &v
	}
	return out
}

// slackTeamURL returns the workspace URL from auth.test when it is an https
// URL on a slack.com host, and "" otherwise. It is only displayed.
func slackTeamURL(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.User != nil || len(v) > 2048 || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".slack.com") {
		return ""
	}
	return v
}

// slackSettingsInput is the part of a PUT or PATCH body with the settings.
type slackSettingsInput struct {
	ImmediateNotify json.RawMessage `json:"immediate_notify"`
	DetectMentions  json.RawMessage `json:"detect_mentions"`
	DetectDMs       json.RawMessage `json:"detect_dms"`
	DetectGroupDMs  json.RawMessage `json:"detect_group_dms"`
}

// settings decodes the settings present in the body. It reports false and
// writes the response when one is not a boolean.
func (in slackSettingsInput) settings(w http.ResponseWriter) (store.SlackSettings, int, bool) {
	var out store.SlackSettings
	n := 0
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		dst  **bool
	}{{"immediate_notify", in.ImmediateNotify, &out.ImmediateNotify}, {"detect_mentions", in.DetectMentions, &out.DetectMentions}, {"detect_dms", in.DetectDMs, &out.DetectDMs}, {"detect_group_dms", in.DetectGroupDMs, &out.DetectGroupDMs}} {
		if f.raw == nil {
			continue
		}
		v, ok := decodeBool(f.raw)
		if !ok {
			fail(w, 400, "invalid_request", f.name+" must be a boolean.")
			return store.SlackSettings{}, 0, false
		}
		*f.dst = &v
		n++
	}
	return out, n, true
}

func (s *Server) handleGetSlack(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	c, err := s.auth.Store.SlackConnection(r.Context(), u.ID)
	if errors.Is(err, store.ErrConnectionNotFound) {
		writeJSON(w, 200, githubDisconnectedResponse{})
		return
	}
	if err != nil {
		integrationError(w, "slack get", u.ID, err)
		return
	}
	writeJSON(w, 200, toSlackResponse(c))
}

func (s *Server) handlePutSlack(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in struct {
		Token json.RawMessage `json:"token"`
		slackSettingsInput
	}
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	var token string
	if in.Token != nil {
		token, ok = decodeString(in.Token)
	}
	// The same format check as a GitHub token: printable ASCII.
	if in.Token == nil || !ok || !validGitHubToken(token) {
		fail(w, 400, "invalid_request", "token is invalid.")
		return
	}
	settings, _, ok := in.settings(w)
	if !ok {
		return
	}
	if !strings.HasPrefix(token, "xoxp-") {
		fail(w, 400, "slack_token_invalid", "The Slack token must be a User OAuth Token (xoxp-).")
		return
	}
	// Slack is called before, and outside, the store transaction.
	ctx, cancel := context.WithTimeout(r.Context(), slackTimeout)
	id, scopes, err := s.slack.AuthTest(ctx, token)
	cancel()
	switch {
	case err == nil:
	case errors.Is(err, slack.ErrUnauthorized):
		fail(w, 400, "slack_token_invalid", "The Slack token is invalid or expired.")
		return
	default:
		log.Printf("integrations: slack verify user=%s: %v", u.ID, err)
		fail(w, 503, "slack_unavailable", "Slack could not be reached. Try again later.")
		return
	}
	if !slackIDPattern.MatchString(id.TeamID) || !slackIDPattern.MatchString(id.UserID) {
		log.Printf("integrations: slack verify user=%s: unexpected identity", u.ID)
		fail(w, 503, "slack_unavailable", "Slack could not be reached. Try again later.")
		return
	}
	have := map[string]bool{}
	for _, sc := range scopes {
		have[sc] = true
	}
	var missing []string
	for _, sc := range slacksync.RequiredScopes {
		if !have[sc] {
			missing = append(missing, sc)
		}
	}
	if len(missing) > 0 {
		fail(w, 400, "slack_scopes_missing", "The token is missing required scopes: "+strings.Join(missing, ", ")+".")
		return
	}
	sealed, err := s.box.Seal(slacksync.TokenPurpose(u.ID), token)
	if err != nil {
		integrationError(w, "slack seal", u.ID, err)
		return
	}
	c, err := s.auth.Store.SaveSlackConnection(r.Context(), store.SlackConnection{UserID: u.ID, TokenSealed: sealed, TokenLast4: tokenLast4(token), TeamID: id.TeamID, TeamName: id.Team, TeamURL: slackTeamURL(id.URL),
		SlackUserID: id.UserID, SlackUserName: id.User, Scopes: strings.Join(scopes, ",")}, settings, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "slack save", u.ID, err)
		return
	}
	writeJSON(w, 200, toSlackResponse(c))
}

func (s *Server) handlePatchSlack(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	var in slackSettingsInput
	if !decode(w, r, &in, integrationBodyLimit) {
		return
	}
	settings, n, ok := in.settings(w)
	if !ok {
		return
	}
	if n == 0 {
		fail(w, 400, "invalid_request", "At least one of immediate_notify, detect_mentions, detect_dms and detect_group_dms is required.")
		return
	}
	c, err := s.auth.Store.UpdateSlackSettings(r.Context(), u.ID, settings, sid, s.auth.Now().Unix())
	switch {
	case err == nil:
	case errors.Is(err, store.ErrUnauthorized):
		s.authError(w, r, err)
		return
	case errors.Is(err, store.ErrConnectionNotFound):
		fail(w, 404, "not_found", "Slack is not connected.")
		return
	default:
		integrationError(w, "slack patch", u.ID, err)
		return
	}
	writeJSON(w, 200, toSlackResponse(c))
}

func (s *Server) handleDeleteSlack(w http.ResponseWriter, r *http.Request) {
	u, ok := userFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	sid, ok := sessionFrom(r.Context())
	if !ok {
		s.authError(w, r, store.ErrUnauthorized)
		return
	}
	err := s.auth.Store.DeleteSlackConnection(r.Context(), u.ID, sid, s.auth.Now().Unix())
	if errors.Is(err, store.ErrUnauthorized) {
		s.authError(w, r, err)
		return
	}
	if err != nil {
		integrationError(w, "slack delete", u.ID, err)
		return
	}
	w.WriteHeader(204)
}
