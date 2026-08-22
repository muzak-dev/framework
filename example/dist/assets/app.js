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
