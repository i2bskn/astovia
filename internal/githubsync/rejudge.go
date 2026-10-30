package githubsync

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/i2bskn/astovia/internal/ai"
	"github.com/i2bskn/astovia/internal/github"
	"github.com/i2bskn/astovia/internal/store"
)

// The functions here build the AI input of a mention item again, from its
// issue or pull request read again, to try the user's AI instructions. They
// use the same conversions as the synchronization; reading GitHub is left
// to the caller.

// MentionPrefix starts the source keys of the mention items:
// github:mention:{owner}/{repo}#{number}:{post}.
const MentionPrefix = "github:" + kindMention + ":"

// FallbackPrefix starts the content of the confirmation task made when the
// AI could not judge a mention.
const FallbackPrefix = "メンションを確認: "

// postIDPattern matches the post part of a mention key (see mentionPosts).
var postIDPattern = regexp.MustCompile(`^(body|c[0-9]{1,20}|r[0-9]{1,20}|rc[0-9]{1,20})$`)

// ParseMentionKey parses the source key of a mention item into its issue or
// pull request (owner and repository lower-cased) and the ID of the post.
func ParseMentionKey(key string) (github.Ref, string, bool) {
	rest, ok := strings.CutPrefix(key, MentionPrefix)
	if !ok {
		return github.Ref{}, "", false
	}
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return github.Ref{}, "", false
	}
	rest, post := rest[:i], rest[i+1:]
	repo, number, ok := strings.Cut(rest, "#")
	owner, name, ok2 := strings.Cut(repo, "/")
	n, err := strconv.Atoi(number)
	if !ok || !ok2 || err != nil || n < 1 || strconv.Itoa(n) != number || !github.ValidName(owner) || !github.ValidName(name) || !postIDPattern.MatchString(post) {
		return github.Ref{}, "", false
	}
	return github.Ref{Owner: owner, Repo: name, Number: n}, post, true
}

// RejudgeScope selects the related tasks of the issue or pull request as the
// synchronization does, without the items of exclude.
func RejudgeScope(gref github.Ref, now time.Time, exclude []string) store.RelatedScope {
	scope := relatedScope(gref, now)
	scope.Exclude = exclude
	return scope
}

// RejudgeInput builds the AI input of the judgment of the mention in the
// post postID of the issue or pull request read again. ok is false when the
// post is not among them.
func RejudgeInput(login string, loc *time.Location, gref github.Ref, issue github.Issue, comments []github.Comment, reviews []github.Review, reviewComments []github.Comment, postID string, related []store.RelatedTask) (ai.MentionInput, bool) {
	posts := mentionPosts(issue, comments, reviews, reviewComments)
	for i, p := range posts {
		if p.ID == postID {
			return mentionInput(login, loc, gref, issue, aiPosts(posts), i, related, ai.Instructions{}), true
		}
	}
	return ai.MentionInput{}, false
}
