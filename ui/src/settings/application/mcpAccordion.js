// popgres: the MCP server settings.
export function mcpAccordion(pageData) {
    return t.details(
        {
            pbEvent: "mcpAccordion",
            className: "accordion mcp-accordion",
            name: "settingsAccordion",
        },
        t.summary(
            null,
            t.i({ className: "ri-robot-2-line", ariaHidden: true }),
            t.span({ className: "txt" }, "MCP server"),
            t.div({ className: "flex-fill" }),
            () => {
                if (pageData.formSettings.mcp?.enabled) {
                    return t.span({ className: "label success" }, "Enabled");
                }
                return t.span({ className: "label" }, "Disabled");
            },
        ),
        t.div(
            { className: "grid sm" },
            t.div(
                { className: "col-lg-12" },
                t.div(
                    { className: "field" },
                    t.input({
                        id: "mcp.enabled",
                        name: "mcp.enabled",
                        type: "checkbox",
                        className: "switch",
                        checked: () => pageData.formSettings.mcp?.enabled || false,
                        onchange: (e) => {
                            pageData.formSettings.mcp = pageData.formSettings.mcp || {};
                            pageData.formSettings.mcp.enabled = e.target.checked;
                        },
                    }),
                    t.label({ htmlFor: "mcp.enabled" }, t.span({ className: "txt" }, "Enable")),
                ),
            ),
            t.div(
                { className: "col-lg-12" },
                t.p(
                    { className: "txt-hint" },
                    "Model Context Protocol server (Streamable HTTP) at ",
                    t.code(null, window.location.origin + "/mcp"),
                    ". The clients authenticate with a popgres auth token (",
                    t.code(null, "Authorization: Bearer <token>"),
                    ") and get list, get, create, update, delete and vector search tools for the collections, with the same API rules.",
                ),
            ),
        ),
    );
}
