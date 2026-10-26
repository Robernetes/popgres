// Compatibility tests of the official PocketBase JS SDK against a running popgres server.
//
// Environment variables:
//   POPGRES_URL                         the server url (eg. http://127.0.0.1:8095)
//   POPGRES_EMAIL / POPGRES_PASSWORD    the credentials of an existing superuser
//
// Run with scripts/sdk-test.sh.

import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";
import { EventSource } from "eventsource";
import PocketBase from "pocketbase";

// the SDK realtime service uses the browser EventSource
globalThis.EventSource = EventSource;

const url = process.env.POPGRES_URL || "http://127.0.0.1:8095";
const suffix = Date.now().toString(36);
const postsName = "sdk_posts_" + suffix;

const superuser = new PocketBase(url);
superuser.autoCancellation(false);

function newClient() {
    const pb = new PocketBase(url);
    pb.autoCancellation(false);
    return pb;
}

async function newUser(name) {
    const email = `${name}_${suffix}@example.com`;
    const password = "1234567890";

    const pb = newClient();
    const record = await pb.collection("users").create({ email, password, passwordConfirm: password, name });
    await pb.collection("users").authWithPassword(email, password);

    return { pb, record, email, password };
}

// waitFor resolves with the first realtime event that matches the predicate.
function waitFor(events, predicate, timeout = 10000) {
    return new Promise((resolve, reject) => {
        const started = Date.now();
        const check = () => {
            const found = events.find(predicate);
            if (found) {
                return resolve(found);
            }
            if (Date.now() - started > timeout) {
                return reject(new Error("timeout waiting for the realtime event"));
            }
            setTimeout(check, 50);
        };
        check();
    });
}

let alice, bob;

before(async () => {
    await superuser.collection("_superusers").authWithPassword(process.env.POPGRES_EMAIL, process.env.POPGRES_PASSWORD);

    const users = await superuser.collections.getOne("users");

    await superuser.collections.create({
        name: postsName,
        type: "base",
        fields: [
            { name: "title", type: "text", required: true },
            { name: "published", type: "bool" },
            { name: "views", type: "number" },
            { name: "meta", type: "json" },
            { name: "author", type: "relation", collectionId: users.id, maxSelect: 1 },
            { name: "cover", type: "file", maxSelect: 1, maxSize: 1024 * 1024 },
            { name: "created", type: "autodate", onCreate: true },
            { name: "updated", type: "autodate", onCreate: true, onUpdate: true },
        ],
        listRule: "published = true || author = @request.auth.id",
        viewRule: "published = true || author = @request.auth.id",
        createRule: "@request.auth.id != '' && author = @request.auth.id",
        updateRule: "author = @request.auth.id",
        deleteRule: "author = @request.auth.id",
    });

    alice = await newUser("alice");
    bob = await newUser("bob");
});

after(async () => {
    alice?.pb.realtime.unsubscribe();
    bob?.pb.realtime.unsubscribe();

    await superuser.collections.delete(postsName).catch(() => {});
    for (const user of [alice, bob]) {
        if (user) {
            await superuser.collection("users").delete(user.record.id).catch(() => {});
        }
    }
});

describe("auth", () => {
    test("auth with password returns a valid token", async () => {
        assert.ok(alice.pb.authStore.isValid);
        assert.equal(alice.pb.authStore.record.email, alice.email);
    });

    test("auth refresh", async () => {
        const result = await alice.pb.collection("users").authRefresh();
        assert.ok(result.token);
        assert.equal(result.record.id, alice.record.id);
    });

    test("invalid credentials", async () => {
        const pb = newClient();
        await assert.rejects(pb.collection("users").authWithPassword(alice.email, "wrong-password"), (err) => {
            assert.equal(err.status, 400);
            return true;
        });
    });

    test("auth methods", async () => {
        const methods = await newClient().collection("users").listAuthMethods();
        assert.equal(methods.password.enabled, true);
    });

    test("the hidden fields are not returned", async () => {
        const record = await alice.pb.collection("users").getOne(alice.record.id);
        assert.equal(record.password, undefined);
        assert.equal(record.tokenKey, undefined);
        assert.equal(record.email, alice.email); // own record
    });
});

describe("records", () => {
    let created;

    test("create with a file", async () => {
        const data = new FormData();
        data.append("title", "hello world");
        data.append("published", "true");
        data.append("views", "10");
        data.append("meta", JSON.stringify({ tags: ["a", "b"] }));
        data.append("author", alice.record.id);
        data.append("cover", new Blob(["popgres file content"], { type: "text/plain" }), "cover.txt");

        created = await alice.pb.collection(postsName).create(data);

        assert.equal(created.title, "hello world");
        assert.equal(created.published, true);
        assert.equal(created.views, 10);
        assert.deepEqual(created.meta, { tags: ["a", "b"] });
        assert.match(created.cover, /^cover_\w+\.txt$/);
        assert.match(created.created, /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}Z$/);
    });

    test("download the file", async () => {
        const res = await fetch(alice.pb.files.getURL(created, created.cover));
        assert.equal(res.status, 200);
        assert.equal(await res.text(), "popgres file content");
    });

    test("create rule", async () => {
        await assert.rejects(
            bob.pb.collection(postsName).create({ title: "fake", author: alice.record.id }),
            (err) => err.status === 400,
        );
        await assert.rejects(
            newClient().collection(postsName).create({ title: "guest" }),
            (err) => err.status === 400,
        );
    });

    test("view and expand", async () => {
        // the expanded relations follow their own collection rules
        // (another user can't view alice in the users collection)
        const record = await bob.pb.collection(postsName).getOne(created.id, { expand: "author" });
        assert.equal(record.id, created.id);
        assert.equal(record.expand?.author, undefined);

        const own = await alice.pb.collection(postsName).getOne(created.id, { expand: "author" });
        assert.equal(own.expand.author.id, alice.record.id);
    });

    test("update and delete rules", async () => {
        await assert.rejects(
            bob.pb.collection(postsName).update(created.id, { title: "hacked" }),
            (err) => err.status === 404,
        );

        const updated = await alice.pb.collection(postsName).update(created.id, { title: "updated", "views+": 5 });
        assert.equal(updated.title, "updated");
        assert.equal(updated.views, 15);

        await assert.rejects(bob.pb.collection(postsName).delete(created.id), (err) => err.status === 404);
    });

    test("list, filters, sort and pagination", async () => {
        for (let i = 0; i < 5; i++) {
            await alice.pb.collection(postsName).create({
                title: `post ${i}`,
                published: i % 2 === 0,
                views: i,
                author: alice.record.id,
            });
        }

        // the guests see only the published records
        const guest = await newClient().collection(postsName).getList(1, 50, { sort: "views" });
        assert.ok(guest.items.every((r) => r.published));
        assert.equal(guest.totalItems, 4);

        // the author sees all records
        const all = await alice.pb.collection(postsName).getFullList({ sort: "-views,title" });
        assert.equal(all.length, 6);
        assert.equal(all[0].views, 15);

        const filtered = await alice.pb.collection(postsName).getList(1, 2, {
            filter: alice.pb.filter("title ~ {:q} && views >= {:min}", { q: "post", min: 2 }),
            sort: "views",
        });
        assert.equal(filtered.totalItems, 3);
        assert.equal(filtered.totalPages, 2);
        assert.deepEqual(filtered.items.map((r) => r.views), [2, 3]);

        const first = await alice.pb.collection(postsName).getFirstListItem('meta.tags ~ "b"');
        assert.equal(first.id, created.id);

        const fields = await alice.pb.collection(postsName).getList(1, 1, { fields: "id,title", skipTotal: true });
        assert.deepEqual(Object.keys(fields.items[0]).sort(), ["id", "title"]);
        assert.equal(fields.totalItems, -1);
    });

    test("delete", async () => {
        await alice.pb.collection(postsName).delete(created.id);
        await assert.rejects(alice.pb.collection(postsName).getOne(created.id), (err) => err.status === 404);
    });
});

describe("realtime", () => {
    test("create, update and delete events with the rules", async () => {
        const aliceEvents = [];
        const guestEvents = [];

        const guest = newClient();

        await alice.pb.collection(postsName).subscribe("*", (e) => aliceEvents.push(e));
        await guest.collection(postsName).subscribe("*", (e) => guestEvents.push(e));

        const draft = await alice.pb.collection(postsName).create({ title: "draft", author: alice.record.id });

        const createEvent = await waitFor(aliceEvents, (e) => e.action === "create" && e.record.id === draft.id);
        assert.equal(createEvent.record.title, "draft");

        await alice.pb.collection(postsName).update(draft.id, { published: true });
        await waitFor(aliceEvents, (e) => e.action === "update" && e.record.id === draft.id);

        // the guest receives only the published record events
        await waitFor(guestEvents, (e) => e.action === "update" && e.record.id === draft.id);
        assert.ok(!guestEvents.some((e) => e.action === "create" && e.record.id === draft.id));

        await alice.pb.collection(postsName).delete(draft.id);
        await waitFor(aliceEvents, (e) => e.action === "delete" && e.record.id === draft.id);

        await guest.realtime.unsubscribe();
    });

    test("single record subscription", async () => {
        const record = await alice.pb.collection(postsName).create({ title: "single", author: alice.record.id });

        const events = [];
        await alice.pb.collection(postsName).subscribe(record.id, (e) => events.push(e));

        await alice.pb.collection(postsName).update(record.id, { title: "single updated" });
        const e = await waitFor(events, (e) => e.action === "update");
        assert.equal(e.record.title, "single updated");

        await alice.pb.collection(postsName).unsubscribe(record.id);
    });
});

describe("health", () => {
    test("health check", async () => {
        const health = await newClient().health.check();
        assert.equal(health.code, 200);
    });
});
