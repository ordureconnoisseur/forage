package cache

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/ordureconnoisseur/forager/internal/stashdb"
)

// BackfillGenders records the StashDB gender of un-owned performers appearing
// on cached scenes, so "hide male performers" can act on them. Returns how
// many were resolved this pass.
//
// It exists because the only genders forage knows are the ones Stash reports
// for performers you OWN. The performers a Discover card offers to ADD are by
// definition not owned, so without this the filter would have nothing to
// judge them by and would silently do nothing to the pills it is most for.
//
// Bounded per call and safe to run repeatedly: performers already recorded are
// excluded by the query, so a backlog drains over successive passes and a
// caught-up instance costs one cheap SELECT and no StashDB traffic at all.
func BackfillGenders(ctx context.Context, sdb *stashdb.Client, db *sql.DB, log *slog.Logger, limit int) (int, error) {
	if sdb == nil {
		return 0, nil
	}
	ids, err := PerformersMissingGender(ctx, db, limit)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	genders, err := sdb.PerformerGenders(ctx, ids)
	// Store whatever came back even on a partial failure: the batches that
	// did land are answers, and discarding them would make a flaky StashDB
	// mean the pass never converges.
	if serr := StoreGenders(ctx, db, genders, time.Now().Unix()); serr != nil {
		return 0, serr
	}
	if err != nil {
		if log != nil {
			log.Warn("gender backfill partial", "resolved", len(genders), "asked", len(ids), "err", err)
		}
		return len(genders), err
	}
	if log != nil && len(genders) > 0 {
		log.Info("gender backfill", "resolved", len(genders))
	}
	return len(genders), nil
}
