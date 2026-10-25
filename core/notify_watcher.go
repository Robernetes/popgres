package core

import (
	"log/slog"
	"sync"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/hook"
)

const systemHookIdNotifyWatcher = "__pbNotifyWatcherSystemHook__"

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

// registerNotifyWatcherHooks synchronizes the cached settings and collections
// of the app instances (nodes) that share the same database.
//
// After a successful settings or collection change the node publishes a cluster
// message (LISTEN/NOTIFY) and the other nodes reload their cached state.
func (app *BaseApp) registerNotifyWatcherHooks() {
	onPanic := func(r any) {
		app.Logger().Error("Cluster notify reload panic", slog.Any("error", r))
	}

	reloadSettings := coalesced(func() {
		if !app.IsBootstrapped() {
			return
		}

		app.Logger().Debug("Reloading settings after cluster notify event")

		if err := app.ReloadSettings(); err != nil {
			app.Logger().Warn("Failed to reload app settings after cluster notify", slog.String("error", err.Error()))
		}
	}, onPanic)

	reloadCollections := coalesced(func() {
		if !app.IsBootstrapped() {
			return
		}

		app.Logger().Debug("Reloading cached collections after cluster notify event")

		if err := app.ReloadCachedCollections(); err != nil {
			app.Logger().Warn("Failed to reload cached collections after cluster notify", slog.String("error", err.Error()))
		}
	}, onPanic)

	app.cluster.Subscribe(clusterTopicSettings, func(msg cluster.Message) {
		if msg.NodeId != app.cluster.NodeId() {
			reloadSettings()
		}
	})

	app.cluster.Subscribe(clusterTopicCollections, func(msg cluster.Message) {
		if msg.NodeId != app.cluster.NodeId() {
			reloadCollections()
		}
	})

	// the messages published while the LISTEN connection was down are lost
	app.cluster.OnReconnect(func() {
		reloadSettings()
		reloadCollections()
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
