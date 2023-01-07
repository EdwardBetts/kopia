// Package maintenance manages automatic repository maintenance.
package maintenance

import (
	"context"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/pkg/errors"

	"github.com/kopia/kopia/internal/clock"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/content/index"
	"github.com/kopia/kopia/repo/logging"
)

var log = logging.Module("maintenance")

const maxClockSkew = 5 * time.Minute

// Mode describes the mode of maintenance to perform.
type Mode string

// Supported maintenance modes.
const (
	ModeNone  Mode = "none"
	ModeQuick Mode = "quick"
	ModeFull  Mode = "full"
	ModeAuto  Mode = "auto" // run either quick of full if required by schedule
)

// TaskType identifies the type of a maintenance task.
type TaskType string

// Task IDs.
const (
	TaskSnapshotGarbageCollection = "snapshot-gc"
	TaskDeleteOrphanedBlobsQuick  = "quick-delete-blobs"
	TaskDeleteOrphanedBlobsFull   = "full-delete-blobs"
	TaskRewriteContentsQuick      = "quick-rewrite-contents"
	TaskRewriteContentsFull       = "full-rewrite-contents"
	TaskDropDeletedContentsFull   = "full-drop-deleted-content"
	TaskIndexCompaction           = "index-compaction"
	TaskCleanupLogs               = "cleanup-logs"
	TaskCleanupEpochManager       = "cleanup-epoch-manager"
)

// shouldRun returns Mode if repository is due for periodic maintenance.
func shouldRun(ctx context.Context, rep repo.DirectRepository, p *Params) (Mode, error) {
	if myUsername := rep.ClientOptions().UsernameAtHost(); p.Owner != myUsername {
		log(ctx).Debugf("maintenance owned by another user '%v'", p.Owner)
		return ModeNone, nil
	}

	s, err := GetSchedule(ctx, rep)
	if err != nil {
		return ModeNone, errors.Wrap(err, "error getting status")
	}

	// check full cycle first, as it does more than the quick cycle
	if p.FullCycle.Enabled {
		if !rep.Time().Before(s.NextFullMaintenanceTime) {
			log(ctx).Debugf("due for full maintenance cycle")
			return ModeFull, nil
		}

		log(ctx).Debugf("not due for full maintenance cycle until %v", s.NextFullMaintenanceTime)
	} else {
		log(ctx).Debugf("full maintenance cycle not enabled")
	}

	// no time for full cycle, check quick cycle
	if p.QuickCycle.Enabled {
		if !rep.Time().Before(s.NextQuickMaintenanceTime) {
			log(ctx).Debugf("due for quick maintenance cycle")
			return ModeQuick, nil
		}

		log(ctx).Debugf("not due for quick maintenance cycle until %v", s.NextQuickMaintenanceTime)
	} else {
		log(ctx).Debugf("quick maintenance cycle not enabled")
	}

	return ModeNone, nil
}

func updateSchedule(ctx context.Context, runParams RunParameters) error {
	rep := runParams.rep
	p := runParams.Params

	s, err := GetSchedule(ctx, rep)
	if err != nil {
		return errors.Wrap(err, "error getting schedule")
	}

	switch runParams.Mode {
	case ModeFull:
		// on full cycle, also update the quick cycle
		s.NextFullMaintenanceTime = rep.Time().Add(p.FullCycle.Interval)
		s.NextQuickMaintenanceTime = rep.Time().Add(p.QuickCycle.Interval)
		log(ctx).Debugf("scheduling next full cycle at %v", s.NextFullMaintenanceTime)
		log(ctx).Debugf("scheduling next quick cycle at %v", s.NextQuickMaintenanceTime)

		return SetSchedule(ctx, rep, s)

	case ModeQuick:
		log(ctx).Debugf("scheduling next quick cycle at %v", s.NextQuickMaintenanceTime)
		s.NextQuickMaintenanceTime = rep.Time().Add(p.QuickCycle.Interval)

		return SetSchedule(ctx, rep, s)

	default:
		return nil
	}
}

// RunParameters passes essential parameters for maintenance.
// It is generated by RunExclusive and can't be create outside of its package and
// is required to ensure all maintenance tasks run under an exclusive lock.
type RunParameters struct {
	rep repo.DirectRepositoryWriter

	Mode Mode

	Params *Params

	// timestamp of the last update of maintenance schedule blob
	MaintenanceStartTime time.Time
}

// NotOwnedError is returned when maintenance cannot run because it is owned by another user.
type NotOwnedError struct {
	Owner string
}

func (e NotOwnedError) Error() string {
	return "maintenance must be run by designated user: " + e.Owner
}

// RunExclusive runs the provided callback if the maintenance is owned by local user and
// lock can be acquired. Lock is passed to the function, which ensures that every call to Run()
// is within the exclusive context.
func RunExclusive(ctx context.Context, rep repo.DirectRepositoryWriter, mode Mode, force bool, cb func(ctx context.Context, runParams RunParameters) error) error {
	rep.DisableIndexRefresh()

	ctx = rep.AlsoLogToContentLog(ctx)

	p, err := GetParams(ctx, rep)
	if err != nil {
		return errors.Wrap(err, "unable to get maintenance params")
	}

	if !force && !p.isOwnedByByThisUser(rep) {
		return NotOwnedError{p.Owner}
	}

	if mode == ModeAuto {
		mode, err = shouldRun(ctx, rep, p)
		if err != nil {
			return errors.Wrap(err, "unable to determine if maintenance is required")
		}
	}

	if mode == ModeNone {
		log(ctx).Debugf("not due for maintenance")
		return nil
	}

	lockFile := rep.ConfigFilename() + ".mlock"
	log(ctx).Debugf("Acquiring maintenance lock in file %v", lockFile)

	// acquire local lock on a config file
	l := flock.New(lockFile)

	ok, err := l.TryLock()
	if err != nil {
		return errors.Wrap(err, "error acquiring maintenance lock")
	}

	if !ok {
		log(ctx).Debugf("maintenance is already in progress locally")
		return nil
	}

	defer l.Unlock() //nolint:errcheck

	runParams := RunParameters{rep, mode, p, time.Time{}}

	// update schedule so that we don't run the maintenance again immediately if
	// this process crashes.
	if err = updateSchedule(ctx, runParams); err != nil {
		return errors.Wrap(err, "error updating maintenance schedule")
	}

	bm, err := runParams.rep.BlobReader().GetMetadata(ctx, maintenanceScheduleBlobID)
	if err != nil {
		return errors.Wrap(err, "error getting maintenance blob time")
	}

	runParams.MaintenanceStartTime = bm.Timestamp

	if err = checkClockSkewBounds(runParams); err != nil {
		return errors.Wrap(err, "error checking for clock skew")
	}

	log(ctx).Infof("Running %v maintenance...", runParams.Mode)
	defer log(ctx).Infof("Finished %v maintenance.", runParams.Mode)

	if err := runParams.rep.Refresh(ctx); err != nil {
		return errors.Wrap(err, "error refreshing indexes before maintenance")
	}

	return cb(ctx, runParams)
}

func checkClockSkewBounds(rp RunParameters) error {
	localTime := rp.rep.Time()
	repoTime := rp.MaintenanceStartTime

	clockSkew := repoTime.Sub(localTime)
	if clockSkew < 0 {
		clockSkew = -clockSkew
	}

	if clockSkew > maxClockSkew {
		return errors.Errorf("Clock skew detected: local clock is out of sync with repository timestamp by more than allowed %v (local: %v repository: %v). Refusing to run maintenance.", maxClockSkew, localTime, repoTime) //nolint:revive
	}

	return nil
}

// Run performs maintenance activities for a repository.
func Run(ctx context.Context, runParams RunParameters, safety SafetyParameters) error {
	switch runParams.Mode {
	case ModeQuick:
		return runQuickMaintenance(ctx, runParams, safety)

	case ModeFull:
		return runFullMaintenance(ctx, runParams, safety)

	default:
		return errors.Errorf("unknown mode %q", runParams.Mode)
	}
}

func runQuickMaintenance(ctx context.Context, runParams RunParameters, safety SafetyParameters) error {
	_, ok, emerr := runParams.rep.ContentManager().EpochManager()
	if ok {
		log(ctx).Debugf("quick maintenance not required for epoch manager")
		return nil
	}

	if emerr != nil {
		return errors.Wrap(emerr, "epoch manager")
	}

	s, err := GetSchedule(ctx, runParams.rep)
	if err != nil {
		return errors.Wrap(err, "unable to get schedule")
	}

	if shouldQuickRewriteContents(s, safety) {
		// find 'q' packs that are less than 80% full and rewrite contents in them into
		// new consolidated packs, orphaning old packs in the process.
		if err := runTaskRewriteContentsQuick(ctx, runParams, s, safety); err != nil {
			return errors.Wrap(err, "error rewriting metadata contents")
		}
	} else {
		notRewritingContents(ctx)
	}

	if shouldDeleteOrphanedPacks(runParams.rep.Time(), s, safety) {
		var err error

		// time to delete orphaned blobs after last rewrite,
		// if the last rewrite was full (started as part of full maintenance) we must complete it by
		// running full orphaned blob deletion, otherwise next quick maintenance will start a quick rewrite
		// and we'd never delete blobs orphaned by full rewrite.
		if hadRecentFullRewrite(s) {
			log(ctx).Debugf("Had recent full rewrite - performing full blob deletion.")
			err = runTaskDeleteOrphanedBlobsFull(ctx, runParams, s, safety)
		} else {
			log(ctx).Debugf("Performing quick blob deletion.")
			err = runTaskDeleteOrphanedBlobsQuick(ctx, runParams, s, safety)
		}

		if err != nil {
			return errors.Wrap(err, "error deleting unreferenced metadata blobs")
		}
	} else {
		notDeletingOrphanedBlobs(ctx, s, safety)
	}

	// consolidate many smaller indexes into fewer larger ones.
	if err := runTaskIndexCompactionQuick(ctx, runParams, s, safety); err != nil {
		return errors.Wrap(err, "error performing index compaction")
	}

	if err := runTaskCleanupLogs(ctx, runParams, s); err != nil {
		return errors.Wrap(err, "error cleaning up logs")
	}

	return nil
}

func notRewritingContents(ctx context.Context) {
	log(ctx).Infof("Previous content rewrite has not been finalized yet, waiting until the next blob deletion.")
}

func notDeletingOrphanedBlobs(ctx context.Context, s *Schedule, safety SafetyParameters) {
	left := nextBlobDeleteTime(s, safety).Sub(clock.Now()).Truncate(time.Second)

	log(ctx).Infof("Skipping blob deletion because not enough time has passed yet (%v left).", left)
}

func runTaskCleanupLogs(ctx context.Context, runParams RunParameters, s *Schedule) error {
	return ReportRun(ctx, runParams.rep, TaskCleanupLogs, s, func() error {
		deleted, err := CleanupLogs(ctx, runParams.rep, runParams.Params.LogRetention.OrDefault())

		log(ctx).Infof("Cleaned up %v logs.", len(deleted))

		return err
	})
}

func runTaskCleanupEpochManager(ctx context.Context, runParams RunParameters, s *Schedule) error {
	em, ok, emerr := runParams.rep.ContentManager().EpochManager()
	if emerr != nil {
		return errors.Wrap(emerr, "epoch manager")
	}

	if !ok {
		return nil
	}

	return ReportRun(ctx, runParams.rep, TaskCleanupEpochManager, s, func() error {
		log(ctx).Infof("Cleaning up old index blobs which have already been compacted...")
		return errors.Wrap(em.CleanupSupersededIndexes(ctx), "error cleaning up superseded index blobs")
	})
}

func runTaskDropDeletedContentsFull(ctx context.Context, runParams RunParameters, s *Schedule, safety SafetyParameters) error {
	var safeDropTime time.Time

	if safety.RequireTwoGCCycles {
		safeDropTime = findSafeDropTime(s.Runs[TaskSnapshotGarbageCollection], safety)
	} else {
		safeDropTime = runParams.rep.Time()
	}

	if safeDropTime.IsZero() {
		log(ctx).Infof("Not enough time has passed since previous successful Snapshot GC. Will try again next time.")
		return nil
	}

	log(ctx).Infof("Found safe time to drop indexes: %v", safeDropTime)

	return ReportRun(ctx, runParams.rep, TaskDropDeletedContentsFull, s, func() error {
		return DropDeletedContents(ctx, runParams.rep, safeDropTime, safety)
	})
}

func runTaskRewriteContentsQuick(ctx context.Context, runParams RunParameters, s *Schedule, safety SafetyParameters) error {
	return ReportRun(ctx, runParams.rep, TaskRewriteContentsQuick, s, func() error {
		return RewriteContents(ctx, runParams.rep, &RewriteContentsOptions{
			ContentIDRange: index.AllPrefixedIDs,
			PackPrefix:     content.PackBlobIDPrefixSpecial,
			ShortPacks:     true,
		}, safety)
	})
}

func runTaskRewriteContentsFull(ctx context.Context, runParams RunParameters, s *Schedule, safety SafetyParameters) error {
	return ReportRun(ctx, runParams.rep, TaskRewriteContentsFull, s, func() error {
		return RewriteContents(ctx, runParams.rep, &RewriteContentsOptions{
			ContentIDRange: index.AllIDs,
			ShortPacks:     true,
		}, safety)
	})
}

func runTaskDeleteOrphanedBlobsFull(ctx context.Context, runParams RunParameters, s *Schedule, safety SafetyParameters) error {
	return ReportRun(ctx, runParams.rep, TaskDeleteOrphanedBlobsFull, s, func() error {
		_, err := DeleteUnreferencedBlobs(ctx, runParams.rep, DeleteUnreferencedBlobsOptions{
			NotAfterTime: runParams.MaintenanceStartTime,
		}, safety)
		return err
	})
}

func runTaskDeleteOrphanedBlobsQuick(ctx context.Context, runParams RunParameters, s *Schedule, safety SafetyParameters) error {
	return ReportRun(ctx, runParams.rep, TaskDeleteOrphanedBlobsQuick, s, func() error {
		_, err := DeleteUnreferencedBlobs(ctx, runParams.rep, DeleteUnreferencedBlobsOptions{
			NotAfterTime: runParams.MaintenanceStartTime,
			Prefix:       content.PackBlobIDPrefixSpecial,
		}, safety)
		return err
	})
}

func runFullMaintenance(ctx context.Context, runParams RunParameters, safety SafetyParameters) error {
	s, err := GetSchedule(ctx, runParams.rep)
	if err != nil {
		return errors.Wrap(err, "unable to get schedule")
	}

	if shouldFullRewriteContents(s, safety) {
		// find packs that are less than 80% full and rewrite contents in them into
		// new consolidated packs, orphaning old packs in the process.
		if err := runTaskRewriteContentsFull(ctx, runParams, s, safety); err != nil {
			return errors.Wrap(err, "error rewriting contents in short packs")
		}
	} else {
		notRewritingContents(ctx)
	}

	// rewrite indexes by dropping content entries that have been marked
	// as deleted for a long time
	if err := runTaskDropDeletedContentsFull(ctx, runParams, s, safety); err != nil {
		return errors.Wrap(err, "error dropping deleted contents")
	}

	if shouldDeleteOrphanedPacks(runParams.rep.Time(), s, safety) {
		// delete orphaned packs after some time.
		if err := runTaskDeleteOrphanedBlobsFull(ctx, runParams, s, safety); err != nil {
			return errors.Wrap(err, "error deleting unreferenced blobs")
		}
	} else {
		notDeletingOrphanedBlobs(ctx, s, safety)
	}

	if err := runTaskCleanupLogs(ctx, runParams, s); err != nil {
		return errors.Wrap(err, "error cleaning up logs")
	}

	if err := runTaskCleanupEpochManager(ctx, runParams, s); err != nil {
		return errors.Wrap(err, "error cleaning up epoch manager")
	}

	return nil
}

// shouldRewriteContents returns true if it's currently ok to rewrite contents.
// since each content rewrite will require deleting of orphaned blobs after some time passes,
// we don't want to starve blob deletion by constantly doing rewrites.
func shouldQuickRewriteContents(s *Schedule, safety SafetyParameters) bool {
	latestContentRewriteEndTime := maxEndTime(s.Runs[TaskRewriteContentsFull], s.Runs[TaskRewriteContentsQuick])
	latestBlobDeleteTime := maxEndTime(s.Runs[TaskDeleteOrphanedBlobsFull], s.Runs[TaskDeleteOrphanedBlobsQuick])

	// never did rewrite - safe to do so.
	if latestContentRewriteEndTime.IsZero() || safety.MinRewriteToOrphanDeletionDelay == 0 {
		return true
	}

	return !latestBlobDeleteTime.Before(latestContentRewriteEndTime)
}

// shouldFullRewriteContents returns true if it's currently ok to rewrite contents.
// since each content rewrite will require deleting of orphaned blobs after some time passes,
// we don't want to starve blob deletion by constantly doing rewrites.
func shouldFullRewriteContents(s *Schedule, safety SafetyParameters) bool {
	// NOTE - we're not looking at TaskRewriteContentsQuick here, this allows full rewrite to sometimes
	// follow quick rewrite.
	latestContentRewriteEndTime := maxEndTime(s.Runs[TaskRewriteContentsFull])
	latestBlobDeleteTime := maxEndTime(s.Runs[TaskDeleteOrphanedBlobsFull], s.Runs[TaskDeleteOrphanedBlobsQuick])

	// never did rewrite - safe to do so.
	if latestContentRewriteEndTime.IsZero() || safety.MinRewriteToOrphanDeletionDelay == 0 {
		return true
	}

	return !latestBlobDeleteTime.Before(latestContentRewriteEndTime)
}

// shouldDeleteOrphanedPacks returns true if it's ok to delete orphaned packs.
// it is only safe to do so after >1hr since the last content rewrite finished to ensure
// other clients refresh their indexes.
// rewritten packs become orphaned immediately but if we don't wait before their deletion
// clients who have old indexes cached may be trying to read pre-rewrite blobs.
func shouldDeleteOrphanedPacks(now time.Time, s *Schedule, safety SafetyParameters) bool {
	return !now.Before(nextBlobDeleteTime(s, safety))
}

func nextBlobDeleteTime(s *Schedule, safety SafetyParameters) time.Time {
	latestContentRewriteEndTime := maxEndTime(s.Runs[TaskRewriteContentsFull], s.Runs[TaskRewriteContentsQuick])
	if latestContentRewriteEndTime.IsZero() {
		return time.Time{}
	}

	return latestContentRewriteEndTime.Add(safety.MinRewriteToOrphanDeletionDelay)
}

func hadRecentFullRewrite(s *Schedule) bool {
	return !maxEndTime(s.Runs[TaskRewriteContentsFull]).Before(maxEndTime(s.Runs[TaskRewriteContentsQuick]))
}

func maxEndTime(taskRuns ...[]RunInfo) time.Time {
	var result time.Time

	for _, tr := range taskRuns {
		for _, r := range tr {
			if !r.Success {
				continue
			}

			if r.End.After(result) {
				result = r.End
			}
		}
	}

	return result
}

// findSafeDropTime returns the latest timestamp for which it is safe to drop content entries
// deleted before that time, because at least two successful GC cycles have completed
// and minimum required time between the GCs has passed.
//
// The worst possible case we need to handle is:
//
// Step #1 - race between GC and snapshot creation:
//
//   - 'snapshot gc' runs and marks unreachable contents as deleted
//   - 'snapshot create' runs at approximately the same time and creates manifest
//     which makes some contents live again.
//
// As a result of this race, GC has marked some entries as incorrectly deleted, but we
// can still return them since they are not dropped from the index.
//
// Step #2 - fix incorrectly deleted contents
//
//   - subsequent 'snapshot gc' runs and undeletes contents incorrectly
//     marked as deleted in Step 1.
//
// After Step 2 completes, we know for sure that all contents deleted before Step #1 has started
// are safe to drop from the index because Step #2 has fixed them, as long as all snapshots that
// were racing with snapshot GC in step #1 have flushed pending writes, hence the
// safety.MarginBetweenSnapshotGC.
func findSafeDropTime(runs []RunInfo, safety SafetyParameters) time.Time {
	var successfulRuns []RunInfo

	for _, r := range runs {
		if r.Success {
			successfulRuns = append(successfulRuns, r)
		}
	}

	if len(successfulRuns) <= 1 {
		return time.Time{}
	}

	// sort so that successfulRuns[0] is the latest
	sort.Slice(successfulRuns, func(i, j int) bool {
		return successfulRuns[i].Start.After(successfulRuns[j].Start)
	})

	// Look for previous successful run such that the time between GCs exceeds the safety margin.
	for _, r := range successfulRuns[1:] {
		diff := -r.End.Sub(successfulRuns[0].Start)
		if diff > safety.MarginBetweenSnapshotGC {
			return r.Start.Add(-safety.DropContentFromIndexExtraMargin)
		}
	}

	return time.Time{}
}
