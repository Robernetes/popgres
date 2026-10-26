// {
//     record: undefined,
//     field: undefined,
//     short: false,
// }
export function view(props) {
    return t.div({ className: "record-field-view field-type-vector" }, () => {
        const rawValue = props.record[props.field.name];

        if (!Array.isArray(rawValue) || !rawValue.length) {
            return t.span({ className: "txt-hint" }, "N/A");
        }

        if (props.short) {
            return t.span({
                className: "txt-code txt-ellipsis",
                textContent: app.utils.truncate(
                    "[" + rawValue.slice(0, 5).join(", ") + (rawValue.length > 5 ? ", ..." : "") + "]",
                ),
            });
        }

        return app.components.codeBlock({
            value: () => JSON.stringify(rawValue),
            footnote: rawValue.length + " dimensions",
        });
    });
}
