// popgres: the pgvector "vector" field.
import { input } from "./input";
import { settings } from "./settings";
import { view } from "./view";

window.app = window.app || {};
window.app.fieldTypes = window.app.fieldTypes || {};
window.app.fieldTypes.vector = {
    icon: "ri-bubble-chart-line",
    label: "Vector",
    settings,
    input,
    view,
    dummyData: (f, forSubmit = false) => {
        return Array.from({ length: Math.min(f?.dimensions || 3, 3) }, (_, i) => (i + 1) / 10);
    },
};
