// the field types that could be searched (see core.SearchField)
const sourceTypes = ["text", "editor", "email", "url", "select"];

// the built-in Postgres text search configurations
const languages = [
    "simple",
    "arabic",
    "armenian",
    "basque",
    "catalan",
    "danish",
    "dutch",
    "english",
    "finnish",
    "french",
    "german",
    "greek",
    "hindi",
    "hungarian",
    "indonesian",
    "irish",
    "italian",
    "lithuanian",
    "nepali",
    "norwegian",
    "portuguese",
    "romanian",
    "russian",
    "serbian",
    "spanish",
    "swedish",
    "tamil",
    "turkish",
    "yiddish",
];

// {
//     originalCollection: undefined,
//     collection: undefined,
//     field
//     get fieldIndex: int/-1,
//     get originalField: undefined
// }
export function settings(props) {
    const uniqueId = "f_" + app.utils.randomString();

    const languageOptions = languages.map((name) => {
        return {
            value: name,
            label: name == "simple" ? "simple (no stemming)" : name,
        };
    });

    return app.components.fieldSettings(props, {
        showPresentable: false,
        content: () =>
            t.div(
                { className: "grid sm" },
                t.div(
                    { className: "col-sm-8" },
                    t.div(
                        { className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".fields" },
                            t.span({ className: "txt" }, "Fields to search"),
                            t.i({
                                className: "ri-information-line link-hint",
                                ariaDescription: app.attrs.tooltip(
                                    "The text, editor, email, url and select fields of the collection. The first selected field has the highest relevance.",
                                ),
                            }),
                        ),
                        app.components.select({
                            id: uniqueId + ".fields",
                            name: () => `fields.${props.fieldIndex}.fields`,
                            required: true,
                            max: 8,
                            placeholder: "Select fields*",
                            options: () =>
                                (props.collection?.fields || [])
                                    .filter((f) => {
                                        if (!sourceTypes.includes(f.type) || !f.name) {
                                            return false;
                                        }
                                        // the auth email visibility is checked per record
                                        return !(props.collection?.type == "auth" && f.name == "email");
                                    })
                                    .map((f) => ({ value: f.name, label: f.name })),
                            value: () => props.field.fields || [],
                            onchange: (opts) => {
                                props.field.fields = (opts || []).map((opt) => opt.value);
                            },
                        }),
                    ),
                ),
                t.div(
                    { className: "col-sm-4" },
                    t.div(
                        { className: "field" },
                        t.label(
                            { htmlFor: uniqueId + ".language" },
                            t.span({ className: "txt" }, "Language"),
                            t.i({
                                className: "ri-information-line link-hint",
                                ariaDescription: app.attrs.tooltip(
                                    "The Postgres text search configuration for the word stemming and stop words.",
                                ),
                            }),
                        ),
                        app.components.select({
                            id: uniqueId + ".language",
                            name: () => `fields.${props.fieldIndex}.language`,
                            options: languageOptions,
                            value: () => props.field.language || "simple",
                            onchange: (opts) => {
                                props.field.language = opts?.[0]?.value || "simple";
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
                        "A Postgres generated column with a GIN index, always in sync with the selected fields. ",
                        "It is not returned in the records. Filter with ",
                        t.code(null, `${props.field.name || "field"} ~ 'words'`),
                        " and sort by relevance with ",
                        t.code(null, `-@textRank(${props.field.name || "field"}, 'words')`),
                        ".",
                    ),
                ),
            ),
    });
}
