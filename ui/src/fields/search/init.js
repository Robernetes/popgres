// popgres: the full text "search" field (a generated tsvector column without record value).
import { settings } from "./settings";

window.app = window.app || {};
window.app.fieldTypes = window.app.fieldTypes || {};
window.app.fieldTypes.search = {
    icon: "ri-search-eye-line",
    label: "Search",
    settings,
    dummyData: (f, forSubmit = false) => {
        return undefined; // hide (the field has no value)
    },
};
