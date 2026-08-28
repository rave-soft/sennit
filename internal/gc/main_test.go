package gc

import (
	"os"
	"testing"

	"github.com/rave-soft/sennit/internal/db"
)

// TestMain stamps this package's throwaway databases from one migrated
// template instead of running the migration chain for each of them. See
// db.UseMigratedTemplate: under -race the chain costs ~210ms per database,
// which is most of what a test in here does.
func TestMain(m *testing.M) {
	db.UseMigratedTemplate()
	os.Exit(m.Run())
}
