package borges

import (
	"fmt"
	"strings"
	"time"

	"github.com/inconshreveable/log15"
	"gopkg.in/src-d/core-retrieval.v0/model"
	"gopkg.in/src-d/core-retrieval.v0/repository"
	"gopkg.in/src-d/framework.v0/lock"
	"gopkg.in/src-d/go-errors.v0"
	"gopkg.in/src-d/go-git.v4"
	"gopkg.in/src-d/go-git.v4/config"
	"gopkg.in/src-d/go-git.v4/plumbing"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-kallax.v1"
)

var (
	ErrCleanRepositoryDir     = errors.NewKind("cleaning up local repo dir failed")
	ErrClone                  = errors.NewKind("cloning %s failed")
	ErrPushToRootedRepository = errors.NewKind("push to rooted repo %s failed")
	ErrArchivingRoots         = errors.NewKind("archiving %d out of %d roots failed: %s")
	ErrEndpointsEmpty         = errors.NewKind("endpoints is empty")
	ErrRepositoryIDNotFound   = errors.NewKind("repository id not found: %s")
	ErrChanges                = errors.NewKind("error computing changes")
)

// Archiver archives repositories. Archiver instances are thread-safe and can
// be reused.
//
// See borges documentation for more details about the archiving rules.
type Archiver struct {
	Notifiers struct {
		// Start function, if set, is called whenever a job is started.
		Start func(*Job)
		// Stop function, if set, is called whenever a job stops. If
		// there was an error, it is passed as second parameter,
		// otherwise, it is nil.
		Stop func(*Job, error)
		// Warn function, if set, is called whenever there is a warning
		// during the processing of a repository.
		Warn func(*Job, error)
	}

	// TemporaryCloner is used to clone repositories into temporary storage.
	TemporaryCloner TemporaryCloner

	// RepositoryStore is the database where repository models are stored.
	RepositoryStorage *model.RepositoryStore

	// RootedTransactioner is used to push new references to our repository
	// storage.
	RootedTransactioner repository.RootedTransactioner

	// LockSession is a locker service to prevent concurrent access to the same
	// rooted reporitories.
	LockSession lock.Session
}

func NewArchiver(r *model.RepositoryStore, tx repository.RootedTransactioner,
	tc TemporaryCloner, ls lock.Session) *Archiver {
	return &Archiver{
		TemporaryCloner:     tc,
		RepositoryStorage:   r,
		RootedTransactioner: tx,
		LockSession:         ls,
	}
}

// Do archives a repository according to a job.
func (a *Archiver) Do(j *Job) error {
	a.notifyStart(j)
	err := a.do(j)
	a.notifyStop(j, err)
	return err
}

func (a *Archiver) do(j *Job) (err error) {
	log := log.New("job", j.RepositoryID)
	now := time.Now()

	r, err := a.getRepositoryModel(j)
	if err != nil {
		return err
	}

	log.Debug("repository model obtained",
		"status", r.Status,
		"last-fetch", r.FetchedAt,
		"references", len(r.References))

	endpoint, err := selectEndpoint(r.Endpoints)
	if err != nil {
		return err
	}
	log.Debug("endpoint selected", "endpoint", endpoint)

	gr, err := a.TemporaryCloner.Clone(j.RepositoryID.String(), endpoint)
	if err != nil {
		var finalErr error
		if err != transport.ErrEmptyUploadPackRequest {
			r.FetchErrorAt = &now
			finalErr = ErrClone.Wrap(err, endpoint)
		}

		if err == transport.ErrRepositoryNotFound {
			r.Status = model.NotFound
			finalErr = nil
		}

		if err := a.dbUpdateFailedRepository(r); err != nil {
			return err
		}

		log.Error("error cloning repository", "error", err)
		return finalErr
	}

	defer func() {
		if cErr := gr.Close(); cErr != nil && err == nil {
			err = ErrCleanRepositoryDir.Wrap(cErr)
		}
	}()
	log.Debug("remote repository cloned")

	oldRefs := NewModelReferencer(r)
	newRefs := gr
	changes, err := NewChanges(oldRefs, newRefs)
	if err != nil {
		log.Error("error computing changes", "error", err)
		return ErrChanges.Wrap(err)
	}

	log.Debug("changes obtained", "roots", len(changes))
	if err := a.pushChangesToRootedRepositories(log, j, r, gr, changes, now); err != nil {
		log.Error("repository processed with errors", "error", err)
		return err
	}

	log.Debug("repository processed")
	return nil
}

func (a *Archiver) getRepositoryModel(j *Job) (*model.Repository, error) {
	q := model.NewRepositoryQuery().FindByID(kallax.ULID(j.RepositoryID))
	r, err := a.RepositoryStorage.FindOne(q)
	if err != nil {
		return nil, ErrRepositoryIDNotFound.Wrap(err, j.RepositoryID.String())
	}

	return r, nil
}

func (a *Archiver) notifyStart(j *Job) {
	if a.Notifiers.Start == nil {
		return
	}

	a.Notifiers.Start(j)
}

func (a *Archiver) notifyStop(j *Job, err error) {
	if a.Notifiers.Stop == nil {
		return
	}

	a.Notifiers.Stop(j, err)
}

func (a *Archiver) notifyWarn(j *Job, err error) {
	if a.Notifiers.Warn == nil {
		return
	}

	a.Notifiers.Warn(j, err)
}

func selectEndpoint(endpoints []string) (string, error) {
	if len(endpoints) == 0 {
		return "", ErrEndpointsEmpty.New()
	}

	// TODO check which endpoint to use
	return endpoints[0], nil
}

func (a *Archiver) pushChangesToRootedRepositories(log log15.Logger,
	j *Job, r *model.Repository, tr TemporaryRepository, changes Changes,
	now time.Time) error {

	var failedInits []model.SHA1
	for ic, cs := range changes {
		lock := a.LockSession.NewLocker(ic.String())
		ch, err := lock.Lock()
		if err != nil {
			failedInits = append(failedInits, ic)
			log.Warn("failed to acquire lock", "root", ic.String(), "error", err)
			continue
		}

		if err := a.pushChangesToRootedRepository(r, tr, ic, cs); err != nil {
			err = ErrPushToRootedRepository.Wrap(err, ic.String())
			a.notifyWarn(j, err)
			failedInits = append(failedInits, ic)
			if err := lock.Unlock(); err != nil {
				log.Warn("failed to release lock", "root", ic.String(), "error", err)
			}

			continue
		}

		r.References = updateRepositoryReferences(r.References, cs, ic)
		if err := a.dbUpdateRepository(r, now); err != nil {
			err = ErrPushToRootedRepository.Wrap(err, ic.String())
			a.notifyWarn(j, err)
			failedInits = append(failedInits, ic)
		}

		select {
		case <-ch:
			log.Error("lost the lock", "root", ic.String())
		default:
		}

		if err := lock.Unlock(); err != nil {
			log.Warn("failed to release lock", "root", ic.String(), "error", err)
		}
	}

	return checkFailedInits(changes, failedInits)
}

func (a *Archiver) pushChangesToRootedRepository(r *model.Repository, tr TemporaryRepository, ic model.SHA1, changes []*Command) error {
	tx, err := a.RootedTransactioner.Begin(plumbing.Hash(ic))
	if err != nil {
		return err
	}

	rr, err := git.Open(tx.Storer(), nil)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	return WithInProcRepository(rr, func(url string) error {
		if err := tr.StoreConfig(r); err != nil {
			_ = tx.Rollback()
			return err
		}

		refspecs := a.changesToPushRefSpec(r.ID, changes)
		if err := tr.Push(url, refspecs); err != nil {
			_ = tx.Rollback()
			return err
		}

		return tx.Commit()
	})
}

func (a *Archiver) changesToPushRefSpec(id kallax.ULID, changes []*Command) []config.RefSpec {
	var rss []config.RefSpec
	for _, ch := range changes {
		var rs string
		switch ch.Action() {
		case Create, Update:
			rs = fmt.Sprintf("+%s:%s/%s", ch.New.Name, ch.New.Name, id)
		case Delete:
			rs = fmt.Sprintf(":%s/%s", ch.Old.Name, id)
		default:
			panic("not reachable")
		}

		rss = append(rss, config.RefSpec(rs))
	}

	return rss
}

// Applies all given changes to a slice of References
func updateRepositoryReferences(oldRefs []*model.Reference, commands []*Command, ic model.SHA1) []*model.Reference {
	rbn := refsByName(oldRefs)
	for _, com := range commands {
		switch com.Action() {
		case Delete:
			ref, ok := rbn[com.Old.Name]
			if !ok {
				continue
			}

			if com.Old.Init == ref.Init {
				delete(rbn, com.Old.Name)
			}
		case Create:
			rbn[com.New.Name] = com.New
		case Update:
			oldRef, ok := rbn[com.New.Name]
			if !ok {
				continue
			}

			if oldRef.Init == com.Old.Init {
				rbn[com.New.Name] = com.New
			}
		}
	}

	// Add the references that keep equals
	var result []*model.Reference
	for _, r := range rbn {
		result = append(result, r)
	}

	return result
}

func (a *Archiver) dbUpdateFailedRepository(repoDb *model.Repository) error {
	_, err := a.RepositoryStorage.Update(repoDb,
		model.Schema.Repository.UpdatedAt,
		model.Schema.Repository.FetchErrorAt,
		model.Schema.Repository.References,
		model.Schema.Repository.Status,
	)

	return err
}

// Updates DB: status, fetch time, commit time
func (a *Archiver) dbUpdateRepository(repoDb *model.Repository, then time.Time) error {

	repoDb.Status = model.Fetched
	repoDb.FetchedAt = &then
	repoDb.LastCommitAt = lastCommitTime(repoDb.References)

	_, err := a.RepositoryStorage.Update(repoDb,
		model.Schema.Repository.UpdatedAt,
		model.Schema.Repository.FetchedAt,
		model.Schema.Repository.LastCommitAt,
		model.Schema.Repository.Status,
		model.Schema.Repository.References,
	)

	return err
}

func lastCommitTime(refs []*model.Reference) *time.Time {
	if len(refs) == 0 {
		return nil
	}

	var last time.Time
	for _, ref := range refs {
		if last.Before(ref.Time) {
			last = ref.Time
		}
	}

	return &last
}

func checkFailedInits(changes Changes, failed []model.SHA1) error {
	n := len(failed)
	if n == 0 {
		return nil
	}

	strs := make([]string, n)
	for i := 0; i < n; i++ {
		strs[i] = failed[i].String()
	}

	return ErrArchivingRoots.New(
		n,
		len(changes),
		strings.Join(strs, ", "),
	)
}

// NewArchiverWorkerPool creates a new WorkerPool that uses an Archiver to
// process jobs. It takes optional start, stop and warn notifier functions that
// are equal to the Archiver notifiers but with additional WorkerContext.
func NewArchiverWorkerPool(r *model.RepositoryStore,
	tx repository.RootedTransactioner,
	tc TemporaryCloner,
	ls lock.Service,
	start func(*WorkerContext, *Job),
	stop func(*WorkerContext, *Job, error),
	warn func(*WorkerContext, *Job, error)) *WorkerPool {

	do := func(ctx *WorkerContext, j *Job) error {
		lsess, err := ls.NewSession(&lock.SessionConfig{TTL: 10 * time.Second})
		if err != nil {
			return err
		}

		a := NewArchiver(r, tx, tc, lsess)

		if start != nil {
			a.Notifiers.Start = func(j *Job) {
				start(ctx, j)
			}
		}

		if stop != nil {
			a.Notifiers.Stop = func(j *Job, err error) {
				stop(ctx, j, err)
			}
		}

		if warn != nil {
			a.Notifiers.Warn = func(j *Job, err error) {
				warn(ctx, j, err)
			}
		}

		return a.Do(j)
	}

	return NewWorkerPool(do)
}
