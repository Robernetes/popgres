package core

import (
	"log/slog"
	"sync"
	"time"

	"github.com/Robernetes/popgres/core/cluster"
	"github.com/Robernetes/popgres/tools/hook"
)

const systemHookIdNotifyWatcher = "__pbNotifyWatcherSystemHook__"

// the cluster topics of the cached app state changes
const (
	clusterTopicSettings    = "settings"
	clusterTopicCollections = "collections"
)

// notifyWatcherDebounce is the delay used to group the reloads of
// multiple consecutive changes (eg. an import of several collections).
const notifyWatcherDebounce = 50 * time.Millisecond

// debounced returns a function that calls fn notifyWatcherDebounce after its last call.
func debounced(fn func()) func() {
	var mu sync.Mutex
	var timer *time.Timer

	return func() {
		mu.Lock()
		defer mu.Unlock()

		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(notifyWatcherDebounce, fn)
	}
}

// registerNotifyWatcherHooks synchronizes the cached settings and collections
// of the app instances (nodes) that share the same database.
//
// After a successful settings or collection change the node publishes a cluster
// message (LISTEN/NOTIFY) and the other nodes reload their cached state.
func (app *BaseApp) registerNotifyWatcherHooks() {
	reloadSettings := debounced(func() {
		if !app.IsBootstrapped() {
			return
		}

		app.Logger().Debug("Reloading settings after cluster notify event")

		if err := app.ReloadSettings(); err != nil {
			app.Logger().Warn("Failed to reload app settings after cluster notify", slog.String("error", err.Error()))
		}
	})

	reloadCollections := debounced(func() {
		if !app.IsBootstrapped() {
			return
		}

		app.Logger().Debug("Reloading cached collections after cluster notify event")

		if err := app.ReloadCachedCollections(); err != nil {
			app.Logger().Warn("Failed to reload cached collections after cluster notify", slog.String("error", err.Error()))
		}
	})

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
