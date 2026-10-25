// popgres: read-only info about the realtime events source and the
// Postgres logical replication slot metrics.
export function realtimeAccordion() {
    const info = store({
        isLoading: false,
        source: "",
        slot: null,
        retainedWALWarning: false,
    });

    async function loadInfo() {
        info.isLoading = true;

        try {
            const health = await app.pb.health.check({ requestKey: "loadRealtimeInfo" });

            info.source = health.data?.realtime?.source || "";
            info.slot = health.data?.realtime?.slot || null;
            info.retainedWALWarning = !!health.data?.realtime?.retainedWALWarning;
            info.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
                info.isLoading = false;
            }
        }
    }

    function formatBytes(bytes) {
        bytes = bytes << 0;
        if (bytes < 1024) {
            return bytes + " B";
        }
        const units = ["KB", "MB", "GB", "TB"];
        let value = bytes;
        let unit = -1;
        while (value >= 1024 && unit < units.length - 1) {
            value /= 1024;
            unit++;
        }
        return value.toFixed(1) + " " + units[unit];
    }

    return t.details(
        {
            pbEvent: "realtimeAccordion",
            className: "accordion realtime-accordion",
            name: "settingsAccordion",
            onmount: () => {
                loadInfo();
            },
        },
        t.summary(
            null,
            t.i({ className: "ri-broadcast-line", ariaHidden: true }),
            t.span({ className: "txt" }, "Realtime"),
            () => {
                if (info.retainedWALWarning) {
                    return t.i({
                        className: "ri-alert-line txt-warning",
                        ariaDescription: app.attrs.tooltip(
                            "The replication slot retains a large amount of WAL.",
                            "right",
                        ),
                    });
                }
            },
            t.div({ className: "flex-fill" }),
            () => {
                if (info.isLoading) {
                    return t.span({ className: "loader sm" });
                }
                if (info.source == "replication") {
                    return t.span({ className: "label success" }, "Replication");
                }
                return t.span({ className: "label" }, "Hooks");
            },
        ),
        t.div(
            { className: "content m-b-sm" },
            t.p(
                null,
                "The realtime record events source is set with the POPGRES_REALTIME_SOURCE env variable. ",
                "With \"replication\" the changes made outside of the API (eg. with direct SQL) are also broadcasted.",
            ),
            () => {
                if (!info.slot) {
                    return t.p({ className: "txt-hint" }, "There is no replication slot.");
                }

                return t.div(
                    { className: info.retainedWALWarning ? "alert warning" : "alert info" },
                    t.div(
                        null,
                        t.div(null, t.span(null, "Slot: "), t.strong(null, info.slot.slot)),
                        t.div(
                            null,
                            t.span(null, "Status: "),
                            t.strong(
                                null,
                                (info.slot.active ? "active" : "inactive")
                                    + (info.slot.walStatus ? " (" + info.slot.walStatus + ")" : ""),
                            ),
                        ),
                        t.div(null, t.span(null, "Lag: "), t.strong(null, formatBytes(info.slot.lagBytes))),
                        t.div(
                            null,
                            t.span(null, "Retained WAL: "),
                            t.strong(null, formatBytes(info.slot.retainedBytes)),
                        ),
                    ),
                );
            },
        ),
    );
}
