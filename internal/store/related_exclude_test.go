package store_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/i2bskn/astovia/internal/store"
)

// RelatedScope.Exclude leaves items out on their own and among the
// subtasks of their group, before Limit; an empty one changes nothing.
func TestRelatedTasksExclude(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			ctx := context.Background()
			s, _ := apiTokenFixture(t, driver)
			c := saveSlack(t, s, "alice-id", "T1", "U1", store.SlackSettings{}, tokenTestNow)
			items := []store.ActionItem{
				slackItem("plain", "slack:T1:m:C1:9.000001:9.000002", 100),
				slackItem("group", "slack:T1:g:C1:9.000001:x", 110),
				slackItem("sub1", "slack:T1:m:C1:9.000001:9.000003", 120),
				slackItem("sub2", "slack:T1:m:C1:9.000001:9.000004", 130),
				slackItem("root", "slack:T1:m:C1:-:9.000001", 90),
				slackItem("dm", "slack:T1:d:C1:9.000001:9.000006:9.000007", 140),
			}
			items[2].ParentID = sql.NullString{String: "group", Valid: true}
			items[3].ParentID = sql.NullString{String: "group", Valid: true}
			err := s.SlackSync(ctx, "alice-id", c.Generation, func(tx *store.SlackTx) error {
				for _, item := range items {
					if _, e := tx.CreateItem(item); e != nil {
						return e
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			scope := store.RelatedScope{
				Source:        store.SourceSlack,
				Prefixes:      []string{"slack:T1:m:C1:9.000001:", "slack:T1:d:C1:9.000001:", "slack:T1:g:C1:9.000001:"},
				ExactKeys:     []string{"slack:T1:m:C1:-:9.000001"},
				GroupPrefixes: []string{"slack:T1:g:C1:9.000001:"},
				FetchLimit:    30, Limit: 10, SubtaskLimit: 5,
			}
			all := []store.RelatedTask{
				{ID: "dm", Content: "Slack: dm"},
				{ID: "group", Content: "Slack: group", Subtasks: []string{"Slack: sub1", "Slack: sub2"}, Group: true},
				{ID: "plain", Content: "Slack: plain"},
				{ID: "root", Content: "Slack: root"},
			}
			for _, exclude := range [][]string{nil, {}} {
				scope.Exclude = exclude
				if got, err := s.RelatedTasks(ctx, "alice-id", scope); err != nil || !reflect.DeepEqual(got, all) {
					t.Fatalf("exclude %v:\ngot  %+v\nwant %+v %v", exclude, got, all, err)
				}
			}
			// A subtask is left out of its group, which stays.
			scope.Exclude = []string{"sub2", "root"}
			want := []store.RelatedTask{
				{ID: "dm", Content: "Slack: dm"},
				{ID: "group", Content: "Slack: group", Subtasks: []string{"Slack: sub1"}, Group: true},
				{ID: "plain", Content: "Slack: plain"},
			}
			if got, err := s.RelatedTasks(ctx, "alice-id", scope); err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("exclude subtask:\ngot  %+v\nwant %+v %v", got, want, err)
			}
			// The items are left out before the limit.
			scope.Exclude, scope.Limit = []string{"dm"}, 2
			if got, err := s.RelatedTasks(ctx, "alice-id", scope); err != nil || len(got) != 2 || got[0].ID != "group" || got[1].ID != "plain" {
				t.Fatalf("exclude before the limit: %+v %v", got, err)
			}
		})
	}
}
