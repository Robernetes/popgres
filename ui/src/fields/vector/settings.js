// {
//     originalCollection: undefined,
//     collection: undefined,
//     field
//     get fieldIndex: int/-1,
//     get originalField: undefined
// }
export function settings(props) {
    const uniqueId = "f_" + app.utils.randomString();

    const metricOptions = [
        { value: "cosine", label: "Cosine distance" },
        { value: "l2", label: "Euclidean (L2) distance" },
        { value: "ip", label: "Inner product" },
    ];

    return app.components.fieldSettings(props, {
        content: () =>
            t.div(
                { className: "grid sm" },
                t.div(
                    { className: "col-sm-6" },
                    t.div(
                        { className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".dimensions" },
                            t.span({ className: "txt" }, "Dimensions"),
                            t.i({
                                className: "ri-information-line link-hint",
                                ariaDescription: app.attrs.tooltip(
                                    "The number of the vector items (eg. 1536). It cannot be changed after the field is created.",
                                ),
                            }),
                        ),
                        t.input({
                            type: "number",
                            id: uniqueId + ".dimensions",
                            name: () => `fields.${props.fieldIndex}.dimensions`,
                            min: 1,
                            max: 16000,
                            step: 1,
                            required: true,
                            disabled: () => !!props.originalField?.id,
                            value: () => props.field.dimensions || "",
                            oninput: (e) => (props.field.dimensions = parseInt(e.target.value, 10) || 0),
                        }),
                    ),
                ),
                t.div(
                    { className: "col-sm-6" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + ".metric" }, "Distance metric"),
                        app.components.select({
                            id: uniqueId + ".metric",
                            name: () => `fields.${props.fieldIndex}.metric`,
                            options: metricOptions,
                            value: () => props.field.metric || "cosine",
                            onchange: (selectedOpts) => {
                                props.field.metric = selectedOpts?.[0]?.value || "cosine";
                            },
                        }),
                    ),
                ),
                t.div(
                    { className: "col-sm-12" },
                    t.div(
                        { className: "field" },
                        t.label({ htmlFor: uniqueId + ".help" }, "Help text"),
                        t.input({
                            type: "text",
                            id: uniqueId + ".help",
                            name: () => `fields.${props.fieldIndex}.help`,
                            value: () => props.field.help || "",
                            oninput: (e) => (props.field.help = e.target.value),
                        }),
                    ),
                ),
                t.div(
                    { className: "col-sm-12" },
                    t.div(
                        { className: "alert info" },
                        "Requires the pgvector extension on the Postgres server. ",
                        "popgres does not generate the embeddings: send them from the client or a hook. ",
                        "Search the closest records with ",
                        t.code(null, "POST /api/collections/{collection}/vector-search"),
                        " or sort with ",
                        t.code(null, "@vectorDistance(field, [...])"),
                        ".",
                    ),
                ),
            ),
        footer: () => [
            t.div(
                { className: "field" },
                t.input({
                    className: "sm",
                    type: "checkbox",
                    id: uniqueId + ".required",
                    name: () => `fields.${props.fieldIndex}.required`,
                    checked: () => !!props.field.required,
                    onchange: (e) => (props.field.required = e.target.checked),
                }),
                t.label({ htmlFor: uniqueId + ".required" }, t.span({ className: "txt" }, "Required")),
            ),
            t.div(
                { className: "field" },
                t.input({
                    className: "sm",
                    type: "checkbox",
                    id: uniqueId + ".index",
                    name: () => `fields.${props.fieldIndex}.index`,
                    checked: () => !!props.field.index,
                    onchange: (e) => (props.field.index = e.target.checked),
                }),
                t.label(
                    { htmlFor: uniqueId + ".index" },
                    t.span({ className: "txt" }, "HNSW index"),
                    t.i({
                        className: "ri-information-line link-hint",
                        ariaDescription: app.attrs.tooltip(
                            "Approximate nearest neighbor index for the selected metric (up to 2000 dimensions).",
                        ),
                    }),
                ),
            ),
        ],
    });
}
