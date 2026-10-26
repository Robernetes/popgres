// {
//     collection: undefined,
//     originalRecord: undefined,
//     record: undefined,
//     field: undefined,
// }
export function input(props) {
    const uniqueId = "vector_" + app.utils.randomString();

    const local = store({
        value: "",
        invalid: false,
    });

    const watchers = [
        watch(
            () => props.record[props.field.name],
            (newVal) => {
                if (Array.isArray(newVal) && newVal.length) {
                    const serialized = JSON.stringify(newVal);
                    if (serialized != local.value.replace(/\s/g, "")) {
                        local.value = serialized;
                    }
                } else if (!local.invalid) {
                    local.value = "";
                }
            },
        ),
    ];

    function updateRecordValue(raw) {
        local.value = raw;

        const trimmed = raw.trim();
        if (trimmed === "") {
            local.invalid = false;
            props.record[props.field.name] = null;
            return;
        }

        try {
            const parsed = JSON.parse(trimmed);
            if (!Array.isArray(parsed) || parsed.some((n) => typeof n != "number")) {
                throw new Error("not an array of numbers");
            }
            local.invalid = false;
            props.record[props.field.name] = parsed;
        } catch (_) {
            // submitted as it is and validated by the server
            local.invalid = true;
            props.record[props.field.name] = trimmed;
        }
    }

    return t.div(
        {
            className: "record-field-input field-type-vector",
            onunmount: () => watchers.forEach((w) => w?.unwatch()),
        },
        t.div(
            { className: "field" },
            t.label(
                { htmlFor: uniqueId },
                t.i({ className: app.fieldTypes.vector.icon, ariaHidden: true }),
                t.span({ className: "txt" }, () => props.field.name),
                t.small({ className: "txt-hint" }, () => `(${props.field.dimensions || "?"} dimensions)`),
                t.span(
                    {
                        hidden: () => !local.invalid,
                        className: "json-state",
                        ariaDescription: app.attrs.tooltip("Expected an array of numbers", "left"),
                    },
                    t.i({ className: "ri-error-warning-fill txt-danger", ariaHidden: true }),
                ),
            ),
            t.textarea({
                id: uniqueId,
                rows: 2,
                name: () => props.field.name,
                required: () => props.field.required,
                placeholder: "[0.12, -0.5, 0.33, ...]",
                value: () => local.value,
                oninput: (e) => updateRecordValue(e.target.value),
            }),
        ),
        () => {
            if (props.field.help) {
                return t.div({ className: "field-help" }, props.field.help);
            }
        },
    );
}
