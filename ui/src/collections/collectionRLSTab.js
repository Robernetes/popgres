// popgres: read-only view of the Postgres row level security policies
// compiled from the saved collection API rules.
export function collectionRLSTab(upsertData) {
    const local = store({
        isLoading: false,
        info: null,
        loadedHash: "",
    });

    const statusLabels = {
        translated: { className: "success", text: "Translated" },
        partial: { className: "warning", text: "Partial" },
        denied: { className: "danger", text: "Denied" },
        excluded: { className: "", text: "No external access" },
        superusersOnly: { className: "", text: "Superusers only" },
        notApplicable: { className: "", text: "Not applicable" },
    };

    async function load() {
        const id = upsertData.originalCollection?.id;
        if (!id) {
            local.info = null;
            return;
        }

        local.isLoading = true;

        try {
            local.info = await app.pb.send("/api/collections/" + encodeURIComponent(id) + "/rls", {
                method: "GET",
                requestKey: "loadCollectionRLS",
            });
            local.loadedHash = upsertData.originalCollectionHash;
            local.isLoading = false;
        } catch (err) {
            if (!err.isAbort) {
                app.checkApiError(err);
                local.isLoading = false;
            }
        }
    }

    function statusLabel(status) {
        const label = statusLabels[status] || { className: "", text: status };
        return t.span({ className: "label " + label.className }, label.text);
    }

    return t.div(
        {
            className: "collection-tab-content collection-rls-tab-content",
            onmount: () => {
                load();
            },
        },
        // reload after save
        () => {
            if (local.loadedHash && local.loadedHash != upsertData.originalCollectionHash) {
                setTimeout(load, 0);
            }
        },
        t.p(
            { className: "txt-hint" },
            "The API rules are also enforced by Postgres row level security policies for the external roles ",
            t.code(null, "popgres_anon"),
            " and ",
            t.code(null, "popgres_authenticated"),
            " (eg. direct SQL connections). The auth record is set with ",
            t.code(null, "SELECT popgres.set_claims('<token>')"),
            ".",
        ),
        () => {
            if (upsertData.isNew) {
                return t.div({ className: "alert info" }, "Save the collection to see its policies.");
            }

            if (local.isLoading && !local.info) {
                return t.div({ className: "block txt-center" }, t.span({ className: "loader" }));
            }

            const info = local.info;
            if (!info) {
                return;
            }

            const items = [];

            if (upsertData.hasChanges) {
                items.push(
                    t.div(
                        { className: "alert warning m-b-sm" },
                        "The policies below are generated from the saved rules (the unsaved changes are not included).",
                    ),
                );
            }

            if (info.applyStatus == "failed") {
                items.push(
                    t.div(
                        { className: "alert danger m-b-sm" },
                        "The policies failed to apply and the external roles are denied (see the logs).",
                    ),
                );
            } else if (info.applyStatus == "pending") {
                items.push(t.div({ className: "alert warning m-b-sm" }, "The policies are not applied yet."));
            }

            items.push(
                t.div(
                    { className: "flex gap-sm m-b-sm" },
                    t.span({ className: "txt" }, "Status:"),
                    statusLabel(info.status),
                    t.div({ className: "flex-fill" }),
                    info.strict
                        ? t.span({ className: "label success" }, "Strict mode")
                        : t.span({ className: "label" }, "Strict mode off"),
                ),
            );

            items.push(
                t.table(
                    { className: "table m-b-sm" },
                    t.thead(
                        null,
                        t.tr(null, t.th(null, "Rule"), t.th(null, "Policy"), t.th(null, "Details")),
                    ),
                    t.tbody(
                        null,
                        (info.policies || []).map((p) =>
                            t.tr(
                                null,
                                t.td({ className: "min-width" }, t.strong(null, p.rule)),
                                t.td({ className: "min-width" }, statusLabel(p.status)),
                                t.td(
                                    { className: "txt-hint" },
                                    p.reason
                                        ? t.span({ className: "txt-danger" }, p.reason)
                                        : p.status == "translated"
                                        ? (p.inline
                                            ? "Inline expression"
                                            : "SECURITY DEFINER function (evaluated per row)")
                                        : "",
                                ),
                            )
                        ),
                    ),
                ),
            );

            items.push(
                app.components.codeBlock({
                    language: "sql",
                    value: info.sql || "",
                }),
            );

            return items;
        },
    );
}
