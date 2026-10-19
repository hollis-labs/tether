package app

import (
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func sessionCorrelation(row store.SessionRow) launch.Correlation {
	return launch.Correlation{TaskID: row.TaskID.String, ProjectID: row.ProjectID, WorkstreamID: row.WorkstreamID.String}
}
