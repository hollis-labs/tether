package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestLaunchCorrelationFillIsAtomic(t *testing.T) {
	s := openWorkstreamStore(t)
	row := appRow("fixed", "", "")
	row.TaskID = sql.NullString{String: "CW-fixed", Valid: true}
	if err := s.CreateSession(row, &launch.Plan{TaskID: "CW-fixed", Env: map[string]string{"KEEP": "yes"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FillSessionLaunchCorrelation(context.Background(), row.ID, "different", "external"); !errors.Is(err, ErrSessionCorrelationConflict) {
		t.Fatal("replacement accepted", err)
	}
	after, err := s.GetSession(row.ID)
	if err != nil || after.WorkstreamID.Valid || after.TaskID.String != "CW-fixed" {
		t.Fatal("conflicting pair partially applied")
	}
	if err := s.CreateSession(appRow("rollback", "", ""), &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.DB().Exec(`CREATE TRIGGER reject_correlation_plan BEFORE UPDATE ON launch_plans WHEN NEW.session_id='rollback' BEGIN SELECT RAISE(ABORT,'test plan write failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FillSessionLaunchCorrelation(context.Background(), "rollback", "CW-new", "external"); err == nil {
		t.Fatal("plan persistence failure ignored")
	}
	after, err = s.GetSession("rollback")
	if err != nil || after.TaskID.Valid || after.WorkstreamID.Valid {
		t.Fatal("row survived failed plan transaction")
	}
	if err := s.FillSessionLaunchCorrelation(context.Background(), "fixed", "", "external"); err != nil {
		t.Fatal(err)
	}
	plan, err := s.GetLaunchPlan("fixed")
	if err != nil || plan.TaskID != "CW-fixed" || plan.WorkstreamID != "external" || plan.Env["KEEP"] != "yes" {
		t.Fatal("fill lost existing plan values")
	}
}
