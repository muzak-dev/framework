// The page this file belongs to is served by app.Frontend, and the data it
// renders comes from the same application's API.
document.cookie = "session_id=demo-session; path=/";

fetch("/feed?token=jessica&limit=3", { headers: { "X-Tag": "go" } })
  .then((response) => response.json())
  .then((feed) => {
    document.getElementById("status").textContent =
      "Reader " + feed.reader + ", " + feed.entries.length + " entries.";
    const list = document.getElementById("feed");
    for (const entry of feed.entries) {
      const item = document.createElement("li");
      item.textContent = entry.title;
      list.append(item);
    }
  })
  .catch((error) => {
    document.getElementById("status").textContent = "The feed could not be read: " + error;
  });

// The item stream is server-sent events, which a browser reads natively: no
// library, no polling, and a reconnection the browser handles itself. The
// server sends the identifier of each event, and the browser sends the last
// one back in Last-Event-ID when it reconnects, which is what lets the stream
// resume rather than start again.
const items = new EventSource("/items/stream?token=jessica");
const list = document.getElementById("items");

items.addEventListener("open", () => {
  document.getElementById("stream-status").textContent = "Streaming. Create an item to see it appear.";
});

// Events are dispatched under the name the server gave them.
items.addEventListener("item_update", (event) => {
  const item = JSON.parse(event.data);
  const row = document.createElement("li");
  row.textContent = item.name + " (" + item.id + ")";
  list.append(row);
});

items.addEventListener("error", () => {
  // A browser reconnects on its own after the delay the server asked for, so
  // there is nothing to do here but say so.
  document.getElementById("stream-status").textContent = "The stream dropped; reconnecting...";
});
