package core

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/hook"
	"github.com/pocketbase/dbx"
)

const systemHookIdNotifyWatcher = "__pbNotifyWatcherSystemHook__"

// popgres: the fsnotify watcher of the local notify dir is replaced by the cluster
// LISTEN/NOTIFY messages (and a periodic resync), so the file is mostly rewritten
// the cluster topics of the cached app state changes
const (
	clusterTopicSettings    = "settings"
	clusterTopicCollections = "collections"
)

// coalesced returns a function that runs fn in the background immediately,
// or once more after the current run if it is already running
// (eg. a reload for multiple consecutive changes like an import of several collections).
//
// A fn panic is recovered and reported with onPanic (eg. the app could be
// cleared while reloading) because it runs in a background goroutine.
func coalesced(fn func(), onPanic func(r any)) func() {
	safeFn := func() {
		defer func() {
			if r := recover(); r != nil {
				onPanic(r)
			}
		}()
		fn()
	}

	var mu sync.Mutex
	var running, pending bool

	return func() {
		mu.Lock()
		if running {
			pending = true
			mu.Unlock()
			return
		}
		running = true
		mu.Unlock()

		go func() {
			for {
				safeFn()

				mu.Lock()
				if !pending {
					running = false
					mu.Unlock()
					return
				}
				pending = false
				mu.Unlock()
			}
		}()
	}
}

// clusterFingerprints are the fingerprints of the cached cluster state (by topic).
type clusterFingerprints struct {
	mu sync.Mutex

	// loaded reports whether the initial state was loaded by the bootstrap
	loaded bool

	values map[string]string
}

// check reports whether the initial state was loaded and whether
// the fingerprint of the topic is the same as the stored one.
func (f *clusterFingerprints) check(topic string, fingerprint string) (loaded bool, unchanged bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.loaded, f.values[topic] == fingerprint
}

func (f *clusterFingerprints) set(topic string, fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[topic] = fingerprint
}

// reset marks the state as not loaded (eg. on bootstrap clear).
func (f *clusterFingerprints) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.loaded = false
	f.values = nil
}

// clusterStateFingerprints returns the current fingerprints of all cluster state topics
// (an empty fingerprint on error, so that the state is reloaded by the next resync).
func (app *BaseApp) clusterStateFingerprints() map[string]string {
	values := map[string]string{}
	for _, topic := range []string{clusterTopicSettings, clusterTopicCollections} {
		values[topic], _ = app.clusterStateFingerprint(topic)
	}
	return values
}

// initClusterFingerprints stores the fingerprints of the state loaded by the bootstrap.
//
// The fingerprints must be taken before the state is loaded (so that the cached
// state is never older than its fingerprint, see registerNotifyWatcherHooks).
func (app *BaseApp) initClusterFingerprints(values map[string]string) {
	app.clusterFingerprints.mu.Lock()
	app.clusterFingerprints.loaded = true
	app.clusterFingerprints.values = values
	app.clusterFingerprints.mu.Unlock()
}

// clusterStateFingerprint returns a hash of the stored state of the cluster topic
// (eg. all collections), so that its cached copy is reloaded only if it changed.
func (app *BaseApp) clusterStateFingerprint(topic string) (string, error) {
	db := app.NonconcurrentDB()
	if db == nil {
		return "", errors.New("the app database is not initialized")
	}

	var fingerprint string
	var err error

	switch topic {
	case clusterTopicSettings:
		err = db.NewQuery("SELECT COALESCE((SELECT md5([[p]]::text) FROM {{_params}} [[p]] WHERE [[p.id]] = {:id}), '')").
			Bind(dbx.Params{"id": paramsKeySettings}).
			Row(&fingerprint)
	case clusterTopicCollections:
		err = db.NewQuery("SELECT COALESCE(md5(string_agg([[c]]::text, ',' ORDER BY [[c.id]])), '') FROM {{_collections}} [[c]]").
			Row(&fingerprint)
	default:
		err = fmt.Errorf("unknown cluster state topic %q", topic)
	}

	return fingerprint, err
}

// registerNotifyWatcherHooks synchronizes the cached settings and collections
// of the app instances (nodes) that share the same database.
//
// After a successful settings or collection change the node publishes a cluster
// message (LISTEN/NOTIFY) and the other nodes reload their cached state.
func (app *BaseApp) registerNotifyWatcherHooks() {
	onPanic := func(r any) {
		app.Logger().Error("Cluster notify reload panic", slog.Any("error", r))
	}

	// reload returns a coalesced function that reloads the cached state of the topic
	// if its fingerprint changed since the last reload.
	//
	// onlyIfChanged is used by the resync (eg. periodically, because a message could be
	// lost) and it waits for the initial state loaded by the bootstrap.
	reload := func(topic string, fn func() error) func(onlyIfChanged bool) {
		var onlyIfChangedMu sync.Mutex
		onlyIfChanged := true

		run := coalesced(func() {
			if !app.IsBootstrapped() {
				return
			}

			onlyIfChangedMu.Lock()
			check := onlyIfChanged
			onlyIfChanged = true
			onlyIfChangedMu.Unlock()

			// note: the fingerprint is taken before the reload so that
			// a change made during the reload is detected by the next resync
			fingerprint, err := app.clusterStateFingerprint(topic)
			if err != nil {
				// eg. the system tables don't exist yet
				app.Logger().Debug("Failed to load the cluster state fingerprint", slog.String("topic", topic), slog.String("error", err.Error()))
				return
			}

			loaded, unchanged := app.clusterFingerprints.check(topic, fingerprint)

			// the resync waits for the initial state loaded by the bootstrap
			// (otherwise it could replace the state that is being loaded)
			if check && !loaded {
				return
			}

			// the cached state is already up to date (eg. a change message and a resync
			// of the same change; the cached state is never older than its fingerprint
			// because the fingerprint is taken before the reload)
			if unchanged {
				return
			}

			app.Logger().Debug("Reloading the cached app state after a cluster change", slog.String("topic", topic))

			if err := fn(); err != nil {
				app.Logger().Warn("Failed to reload the cached app state after a cluster change", slog.String("topic", topic), slog.String("error", err.Error()))
				fingerprint = "" // retry on the next resync
			}

			app.clusterFingerprints.set(topic, fingerprint)
		}, onPanic)

		return func(onlyIfChangedArg bool) {
			if !onlyIfChangedArg {
				onlyIfChangedMu.Lock()
				onlyIfChanged = false
				onlyIfChangedMu.Unlock()
			}
			run()
		}
	}

	reloadSettings := reload(clusterTopicSettings, app.ReloadSettings)
	reloadCollections := reload(clusterTopicCollections, app.ReloadCachedCollections)

	app.cluster.Subscribe(clusterTopicSettings, func(msg cluster.Message) {
		if msg.NodeId != app.cluster.NodeId() {
			reloadSettings(false)
		}
	})

	app.cluster.Subscribe(clusterTopicCollections, func(msg cluster.Message) {
		if msg.NodeId != app.cluster.NodeId() {
			reloadCollections(false)
		}
	})

	// the messages could be lost (eg. while the LISTEN connection was down)
	app.cluster.OnResync(func() {
		reloadSettings(true)
		reloadCollections(true)
	})

	publish := func(topic string) {
		db := app.NonconcurrentDB()
		if db == nil {
			return
		}

		if err := app.cluster.Publish(db, topic, ""); err != nil {
			app.Logger().Warn("Failed to publish cluster notify event", slog.String("topic", topic), slog.String("error", err.Error()))
		}
	}

	// ---------------------------------------------------------------

	// note: the After*Success hooks are triggered only after the transaction commit (if any)
	settingsNotify := func(e *ModelEvent) error {
		err := e.Next()
		if err != nil || e.Model.PK() != paramsKeySettings {
			return err
		}

		// popgres: a cluster message instead of the notify file
		publish(clusterTopicSettings)

		return nil
	}
	app.OnModelAfterCreateSuccess(paramsTable).Bind(&hook.Handler[*ModelEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     settingsNotify,
		Priority: 999,
	})
	app.OnModelAfterUpdateSuccess(paramsTable).Bind(&hook.Handler[*ModelEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     settingsNotify,
		Priority: 999,
	})

	// ---------------------------------------------------------------

	collectionsNotify := func(e *CollectionEvent) error {
		if err := e.Next(); err != nil {
			return err
		}

		// popgres: a cluster message instead of the notify file
		publish(clusterTopicCollections)

		return nil
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     collectionsNotify,
		Priority: 999,
	})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     collectionsNotify,
		Priority: 999,
	})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*CollectionEvent]{
		Id:       systemHookIdNotifyWatcher,
		Func:     collectionsNotify,
		Priority: 999,
	})
}
