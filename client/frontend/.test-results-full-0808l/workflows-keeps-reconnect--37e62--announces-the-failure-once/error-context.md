# Instructions

- Following Playwright test failed.
- Explain why, be concise, respect Playwright best practices.
- Provide a snippet of code with the fix, if possible.

# Test info

- Name: workflows.spec.js >> keeps reconnect countdown changes visual and announces the failure once
- Location: tests\workflows.spec.js:1260:1

# Error details

```
Error: expect(locator).toContainText(expected) failed

Locator: locator('#conn-pill')
Expected substring: "retry 1/5 in 4s"
Received string:    "offline"
Timeout: 5000ms

Call log:
  - Expect "toContainText" with timeout 5000ms
  - waiting for locator('#conn-pill')
    13 × locator resolved to <span class="pill" id="conn-pill">offline</span>
       - unexpected value "offline"

```

```yaml
- dialog "voicx":
  - heading "voicx" [level=1]
  - paragraph: voice ops console
  - text: SERVER
  - textbox "SERVER": 127.0.0.1:12333
  - text: NICKNAME
  - textbox "NICKNAME":
    - /placeholder: nickname or unique id
  - text: PASSWORD
  - textbox "PASSWORD":
    - /placeholder: optional — account login
  - text: SERVER PASSWORD
  - textbox "SERVER PASSWORD":
    - /placeholder: if required
  - button "CONNECT"
  - alert
  - text: RECENT SERVERS
  - region "Recent servers": no recent servers
  - text: Identity auto-generated — stored locally playwright-i… voicx test
- region "Notifications"
```

# Test source

```ts
  1172 | test("cancels stale reconnect batches when switching server tabs", async ({ page }) => {
  1173 |     await page.evaluate(() => {
  1174 |         window.__voicx.showWorkspace(false);
  1175 |         const state = window.__voicx.state;
  1176 |         state.myClientID = "client-a";
  1177 |         state.myUniqueID = "user-a";
  1178 |         state.myNickname = "Alice";
  1179 |         state.myChannelID = 1;
  1180 |         state.settings.chat_notification_level = "all";
  1181 |         state.settings.notify_matrix = {};
  1182 |         state.settings.channel_notify = {};
  1183 |         state.settings.dnd_enabled = false;
  1184 |         window.__resetSpoken = [];
  1185 |         const region = document.getElementById("chat-announcer");
  1186 |         new MutationObserver(() => {
  1187 |             if (region.textContent) window.__resetSpoken.push(region.textContent);
  1188 |         }).observe(region, { childList: true, characterData: true, subtree: true });
  1189 |         const snapshot = () => {
  1190 |             for (const callback of window.__events.snapshot || []) callback(JSON.stringify({
  1191 |                 root_channels: [
  1192 |                     { ChannelID: 1, ParentID: 0, Name: "Lobby", clients: [], children: [] },
  1193 |                 ],
  1194 |             }));
  1195 |         };
  1196 |         const dispatch = (id, text) => {
  1197 |             const message = JSON.stringify({
  1198 |                 type: "chat",
  1199 |                 data: { id, from: "Bob", from_unique_id: "user-b", text, channel_id: 1 },
  1200 |             });
  1201 |             for (const callback of window.__events.event || []) callback(message);
  1202 |         };
  1203 |         snapshot();
  1204 |         window.__voicxChat.beginReconnectAnnouncementBatch(3000);
  1205 |         dispatch(451, "old server replay");
  1206 |         for (const callback of window.__events.tab_reset || []) callback("manual-switch");
  1207 |         state.myClientID = "client-a";
  1208 |         state.myChannelID = 1;
  1209 |         snapshot();
  1210 |         dispatch(452, "new server message");
  1211 |     });
  1212 | 
  1213 |     await expect.poll(() => page.evaluate(() => window.__resetSpoken)).toEqual([
  1214 |         "Bob: new server message",
  1215 |     ]);
  1216 |     await page.waitForTimeout(850);
  1217 |     expect(await page.evaluate(() => window.__resetSpoken.some((text) => text.includes("after reconnect")))).toBe(false);
  1218 | });
  1219 | 
  1220 | test("preserves reconnect batching across the tab created by a real reconnect", async ({ page }) => {
  1221 |     await page.evaluate(() => {
  1222 |         window.__voicx.showWorkspace(false);
  1223 |         const state = window.__voicx.state;
  1224 |         state.settings.chat_notification_level = "all";
  1225 |         state.settings.notify_matrix = {};
  1226 |         state.settings.channel_notify = {};
  1227 |         state.settings.dnd_enabled = false;
  1228 |         window.__preservedReconnectSpoken = [];
  1229 |         const region = document.getElementById("chat-announcer");
  1230 |         new MutationObserver(() => {
  1231 |             if (region.textContent) window.__preservedReconnectSpoken.push(region.textContent);
  1232 |         }).observe(region, { childList: true, characterData: true, subtree: true });
  1233 |         window.__voicxChat.beginReconnectAnnouncementBatch(3000);
  1234 |         state.reconnectInFlight = true;
  1235 |         for (const callback of window.__events.tab_reset || []) callback("reconnected-tab");
  1236 |         state.reconnectInFlight = false;
  1237 |         state.myClientID = "client-a";
  1238 |         state.myUniqueID = "user-a";
  1239 |         state.myNickname = "Alice";
  1240 |         state.myChannelID = 1;
  1241 |         for (const callback of window.__events.snapshot || []) callback(JSON.stringify({
  1242 |             root_channels: [
  1243 |                 { ChannelID: 1, ParentID: 0, Name: "Lobby", clients: [], children: [] },
  1244 |             ],
  1245 |         }));
  1246 |         for (let id = 461; id <= 462; id++) {
  1247 |             const message = JSON.stringify({
  1248 |                 type: "chat",
  1249 |                 data: { id, from: "Bob", from_unique_id: "user-b", text: `replayed ${id}`, channel_id: 1 },
  1250 |             });
  1251 |             for (const callback of window.__events.event || []) callback(message);
  1252 |         }
  1253 |     });
  1254 | 
  1255 |     await expect.poll(() => page.evaluate(() => window.__preservedReconnectSpoken)).toEqual([
  1256 |         "2 messages from Bob received after reconnect",
  1257 |     ]);
  1258 | });
  1259 | 
  1260 | test("keeps reconnect countdown changes visual and announces the failure once", async ({ page }) => {
  1261 |     await page.evaluate(() => {
  1262 |         window.__voicx.showWorkspace(false);
  1263 |         delete window.__voicx.state.settings.reconnect_on_loss;
  1264 |         window.__voicx.state.settings.notify_connection = true;
  1265 |         window.__voicx.state.lastConnect = { addr: "voice.example:12333", nick: "Alice", pw: "", spw: "" };
  1266 |         for (const callback of window.__events.disconnected || []) callback();
  1267 |     });
  1268 |     await expect(page.locator("#conn-pill")).toContainText("retry 1/5 in 5s");
  1269 |     await expect(page.locator("#alert-announcer")).toHaveText("Connection lost");
  1270 |     await expect(page.locator("#conn-pill")).not.toHaveAttribute("aria-live", /.+/);
  1271 |     await page.waitForTimeout(1100);
> 1272 |     await expect(page.locator("#conn-pill")).toContainText("retry 1/5 in 4s");
       |                                              ^ Error: expect(locator).toContainText(expected) failed
  1273 |     await expect(page.locator("#alert-announcer")).toHaveText("Connection lost");
  1274 | });
  1275 | 
  1276 | test("dispatches one DM notification only for actual E2EE direct messages", async ({ page }) => {
  1277 |     await page.evaluate(() => {
  1278 |         const originalNotify = window.__voicxNotify.notify;
  1279 |         window.__notificationDispatches = [];
  1280 |         window.__voicxNotify.notify = (event, text, context) => {
  1281 |             window.__notificationDispatches.push(event);
  1282 |             return originalNotify(event, text, context);
  1283 |         };
  1284 |         Object.defineProperty(document, "hasFocus", { configurable: true, value: () => false });
  1285 |         window.__voicx.state.myNickname = "Alice";
  1286 |         window.__voicx.state.myUniqueID = "user-a";
  1287 |         window.__voicx.state.clients = [
  1288 |             { client_id: "client-b", unique_id: "user-b", nickname: "Bob", channel_id: 0 },
  1289 |             { client_id: "client-c", unique_id: "user-c", nickname: "Carol", channel_id: 0 },
  1290 |         ];
  1291 | 
  1292 |         const direct = JSON.stringify({
  1293 |             type: "chat",
  1294 |             data: {
  1295 |                 id: 801, from: "Bob", from_client_id: "client-b", from_unique_id: "user-b",
  1296 |                 text: "private hello", e2e: true, client_msg_id: "dm-801",
  1297 |             },
  1298 |         });
  1299 |         const global = JSON.stringify({
  1300 |             type: "chat",
  1301 |             data: {
  1302 |                 id: 802, from: "Carol", from_client_id: "client-c", from_unique_id: "user-c",
  1303 |                 text: "global hello", channel_id: 0, e2e: false,
  1304 |             },
  1305 |         });
  1306 |         for (const callback of window.__events.event || []) callback(direct);
  1307 |         for (const callback of window.__events.event || []) callback(global);
  1308 |     });
  1309 | 
  1310 |     await expect.poll(() => page.evaluate(() => window.__notificationDispatches)).toEqual([
  1311 |         "dm",
  1312 |         "channel_message",
  1313 |     ]);
  1314 |     expect(await page.evaluate(() => window.__notificationDispatches.filter((event) => event === "dm").length)).toBe(1);
  1315 |     expect(await page.evaluate(() => window.__calls.TrayMention || 0)).toBe(1);
  1316 |     expect(await page.evaluate(() => window.__voicx.state.lastWhispererUID)).toBe("user-b");
  1317 | });
  1318 | 
  1319 | test("renders hostile update, permission, and image metadata as inert data", async ({ page }) => {
  1320 |     const attack = `<img src=x onerror="document.body.dataset.remoteXss='yes'">`;
  1321 |     await page.evaluate(async (payload) => {
  1322 |         window.__permissions = [{
  1323 |             key: payload,
  1324 |             value: 7,
  1325 |             skip: true,
  1326 |             negate: false,
  1327 |             inherited: true,
  1328 |             source_tier: payload,
  1329 |         }];
  1330 |         window.__updateInfo = { available: true, version: payload, size: 1048576 };
  1331 |         await window.__voicx.refreshPermissions();
  1332 |         await window.__voicx.checkForUpdatesInteractive();
  1333 |     }, attack);
  1334 | 
  1335 |     expect(await page.evaluate(() => document.body.dataset.remoteXss || "")).toBe("");
  1336 |     await expect(page.locator("#perm-area tbody .mono")).toHaveText(attack);
  1337 |     await expect(page.locator("#perm-area tbody tr")).toHaveAttribute("title", `effective from ${attack} (inherited)`);
  1338 |     await expect(page.locator(".upd-status")).toHaveText(`update available: ${attack} (1.0 MiB)`);
  1339 | 
  1340 |     await page.evaluate(async (payload) => {
  1341 |         const host = document.createElement("span");
  1342 |         host.className = "avatar hostile-avatar";
  1343 |         host.dataset.uid = "hostile-user";
  1344 |         document.body.appendChild(host);
  1345 |         window.__avatarResponse = {
  1346 |             content_type: `image/png\" onerror=\"document.body.dataset.remoteXss='image'`,
  1347 |             data_base64: "AAAA",
  1348 |         };
  1349 |         await window.__voicx.fetchAvatar("hostile-user");
  1350 |         window.__voicx.state.avatars.delete("valid-user");
  1351 |         window.__voicx.state.avatarPending.delete("valid-user");
  1352 |         const validHost = document.createElement("span");
  1353 |         validHost.className = "avatar valid-avatar";
  1354 |         validHost.dataset.uid = "valid-user";
  1355 |         document.body.appendChild(validHost);
  1356 |         window.__avatarResponse = { content_type: "image/png", data_base64: "AAAA" };
  1357 |         await window.__voicx.fetchAvatar("valid-user");
  1358 |         void payload;
  1359 |     }, attack);
  1360 | 
  1361 |     expect(await page.evaluate(() => document.body.dataset.remoteXss || "")).toBe("");
  1362 |     await expect(page.locator(".hostile-avatar img")).toHaveCount(0);
  1363 |     expect(await page.evaluate(() => window.__voicx.state.avatars.get("hostile-user"))).toBe(null);
  1364 |     await expect(page.locator(".valid-avatar img")).toHaveAttribute("src", "data:image/png;base64,AAAA");
  1365 | });
  1366 | 
  1367 | test("renders editable permission keys as inert text", async ({ page }) => {
  1368 |     const key = '<span data-permission-key-injection="true">unexpected node</span>';
  1369 |     await page.evaluate(({ permissionKey }) => {
  1370 |         window.__voicx.state.isAdmin = true;
  1371 |         window.__groups = { groups: [{ id: 7, name: "Operators", member_count: 0, color: "" }] };
  1372 |         window.__permEntries = {
```