# Live Events - Actualizări Imediate ale Dashboard-ului

## Prezentare generală

Glance implementează un sistem de **live events cu Server-Sent Events (SSE)** care permite actualizarea imediat a widget-urilor pe dashboard fără necesitatea de refresh manual. Când un serviciu monitorizat se schimbă de stare (ex: DNS pică, container se restartează), modificarea se reflectă instant în pagină.

## Arhitectura

### 1. **Server-Side Components**

#### Event Hub (`internal/glance/events.go`)
- **Tip**: Broadcast hub SSE care gestionează conexiuni client și emit de evenimente
- **Funcții principale**:
  - `newEventHub()` - inițializează hub-ul cu tracker de débounce per widget
  - `register()` / `unregister()` - gestionare conexiuni client
  - `broadcast(msg []byte)` - trimiteEvent la toți clienții conectați
  - `publishEvent(eventType, payload)` - publică un eveniment cu debounce pentru monitor

**Debounce**: Evenimentele `monitor:site_changed` pentru același `widget_id` sunt filtrate - máximum 1 eveniment la 5 secunde per widget (evită flapping).

#### Background Monitor Worker (`internal/glance/glance.go`)
- **Comportament**: Goroutine permanentă care rulează periodic (fiecare 15 secunde)
- **Acțiuni**: Iterează peste toate paginile și widget-urile de tip `monitorWidget`, apelează `update()` pe fiecare
- **Lifecycle**: Se oprește prin context cancellation (`monitorCtx`) la reload config
- **Code location**: Inițiat în `newApplication()` prin `go func()` și folosește `app.monitorCtx`

#### Widget Status Detection (`internal/glance/widget-monitor.go`)
- **Schimbare stare**: Compară `Status` curent cu `PrevStatus` pentru fiecare site
- **Emitere eveniment**: Apelează `publishEvent("monitor:site_changed", {...})` cu payload:
  ```json
  {
    "widget_id": 12345,
    "title": "DNS Server",
    "url": "127.0.0.1:53",
    "status": 0,
    "timed_out": true,
    "error": "i/o timeout"
  }
  ```
- **Câmpuri**: `widget_id`, `title`, `url`, `status`, `timed_out`, `error` (opțional)

#### HTTP Endpoints
1. **`GET /api/events`** - SSE stream
   - Autentificare: validează sesiune (cookies)
   - Headers: `Content-Type: text/event-stream`, `Connection: keep-alive`
   - Keep-alive: ping la 30 secunde
   - Format: JSON cu structura `{type, time, data}`

2. **`GET /api/widgets/{widgetID}/content/`** - Widget partial fetch
   - Returnează HTML randat pentru widget-ul specificat
   - Folosit de client la după primirea evenimentului pentru a reîncărca doar widget-ul
   - Acces: autentificare necesară la fel ca paginile

### 2. **Client-Side Components**

#### SSE Listener (`internal/glance/static/js/page.js`)
- **Inițializare**: Creează `EventSource` la `{baseURL}/api/events`
- **Handlers**:
  - `onmessage`: procesează evenimente JSON
  - `onerror`: reconectare după 3 secunde
- **Fallback**: Dacă SSE nu e suportat, revine la polling periodic (15 secunde)

#### Event Processing
1. **`page:update`** (stare pagină completă schimbată):
   - Fetch-uiește `/api/pages/{slug}/content/`
   - Reîncarcă HTML-ul paginii complete
   - Re-inițializează toate componentele JS (popovers, clocks, etc.)

2. **`monitor:site_changed`** (schimbare stare serviciu):
   - Extrage `widget_id` din payload
   - Fetch-uiește `/api/widgets/{widget_id}/content/`
   - Găsește element-ul DOM cu `[data-widget-id="{widget_id}"]`
   - Înlocuiește (`outerHTML`) doar acel widget
   - Re-inițializează componentele JS locale
   - Fallback: dacă `widget_id` lipsește, fetch-uiește pagina completă

#### DOM Markers
- Template: `internal/glance/templates/widget-base.html`
- Atribut: `data-widget-id="{{ .GetID }}"`
- Folosit de JS pentru a identifica și selecta widget-urile în DOM

### 3. **Lifecycle Management** (`internal/glance/main.go`)
- **Context**: `app.monitorCtx` și `app.monitorCancel`
- **Inițializare**: la `newApplication()` prin `context.WithCancel()`
- **Reload config**: 
  - Background worker al aplicației vechi se oprește prin `oldApp.monitorCancel()`
  - Worker nou al aplicației noi pornește automat
  - Evită goroutine leaks și duplicate monitoring
- **Cleanup**: `defer` în `serveApp()` apelează `cancel()` la ieșire

## Fluxul complet de actualizare

```
┌─────────────────────────────────────────────────────────────────┐
│ Server: Background Monitor Worker (15s interval)                 │
├─────────────────────────────────────────────────────────────────┤
│ 1. Iterează paginile și widget-urile monitorWidget               │
│ 2. Apelează widget.update(ctx) pe fiecare                        │
│ 3. Compară status curent vs. anterior                            │
└────────────────┬──────────────────────────────────────────────────┘
                 │
        ┌────────▼──────────┐
        │ Status changed?   │
        │ (OK → Timeout)    │
        └────────┬──────────┘
                 │ YES
        ┌────────▼──────────────────────────┐
        │ publishEvent("monitor:site_changed")
        │ - widget_id, status, error, etc.  │
        └────────┬─────────────────────────────┘
                 │
        ┌────────▼──────────────────────┐
        │ Debounce Check (5s per widget) │
        │ Filtrează duplicate events     │
        └────────┬──────────────────────┘
                 │
        ┌────────▼──────────────────────┐
        │ Broadcast la EventSource hub   │
        └────────┬──────────────────────┘
                 │
┌────────────────▼──────────────────────────────────────────────┐
│ Client: Browser EventSource Listener                           │
├─────────────────────────────────────────────────────────────────┤
│ 1. Primește {"type":"monitor:site_changed", "data":{...}}       │
│ 2. Extrage widget_id din payload                                │
│ 3. Fetch GET /api/widgets/{widget_id}/content/                  │
│ 4. Găsește [data-widget-id] în DOM                              │
│ 5. Înlocuiește outerHTML cu noul HTML                           │
│ 6. Re-inițializează JS componente (clocks, popovers, etc.)      │
│ 7. Widget se actualizează instant - TIMEOUT apare               │
└────────────────────────────────────────────────────────────────┘
```

## Exemple de comportament

### Exemplu 1: DNS Monitor pică

**Timp 0:00** - Dashboard afișează DNS ca OK
```
[DNS Server] 127.0.0.1:53
Status: OK
Response time: 5ms
```

**Timp 0:15** - Background worker detectează DNS offline
```
→ Status schimbă de (Code: 200) la (TimedOut: true)
→ publishEvent("monitor:site_changed", {
     widget_id: 12345,
     title: "DNS Server",
     status: 0,
     timed_out: true,
     error: "i/o timeout"
})
→ client SSE primește eveniment
→ fetch /api/widgets/12345/content/
→ Widget HTML se înlocuiește
```

**Timp 0:16** - Dashboard se actualizează instant (fără refresh)
```
[DNS Server] 127.0.0.1:53
Status: Timed Out  ← ROȘU, instant
```

### Exemplu 2: Container se restartează

Similar, dar pentru un monitor care verifică port HTTP:
```
[Web Service] http://localhost:8080
Status: Timed Out → OK (in <30s after restart)
```

## Configurare

### Background Monitor Interval
- **Default**: 15 secunde (hard-coded in `glance.go`)
- **Modificabil**: Schimbă `time.NewTicker(15 * time.Second)` în `glance.go` la interval dorit
- **Recomandare**: 10-30 secunde (mai mic = mai responsiv dar mai mult CPU, mai mare = delay mai lung)

### Debounce Window
- **Default**: 5 secunde per widget (hard-coded in `events.go`)
- **Modificabil**: Schimbă `monitorEventDebounceTime: 5 * time.Second` în `events.go`
- **Use case**: Evită spam dacă serviciu alternează OK ↔ Timeout rapid

### SSE Keep-Alive
- **Default**: 30 secunde ping (hard-code in `events.go`)
- **Scop**: Detecta conexiuni morte, bypass reverse proxy timeouts
- **Modificabil**: Schimbă `time.NewTicker(30 * time.Second)` în `handleEvents()`

## Testare

### Setup pentru testare locală

1. **Pornire server**:
   ```bash
   go build -o glance
   ./glance -config docs/glance.yml
   ```

2. **Deschis doua tab-uri de browser**:
   - Tab 1: http://localhost:8080 (main dashboard)
   - Tab 2: http://localhost:8080/api/events (pentru vedere SSE stream - optional)

3. **Simulare schimbare serviciu**:
   ```bash
   # Pornește serviciu pe port 9999 (dacă glance.yml monitorează 127.0.0.1:9999)
   python3 -m http.server 9999 >/dev/null 2>&1 &
   
   # Așteaptă ~15 secunde (interval worker)
   sleep 15
   
   # Oprește serviciu
   kill $!
   sleep 2
   
   # Observ acetat widget-ul în Tab 1 se actualizează instant cu "Timed Out"
   ```

4. **Verificare în console**:
   - F12 → Console tab
   - Căutați mesaje de debug din `page.js`:
     ```javascript
     // Observable:
     "SSE connection established"
     "Event received: monitor:site_changed"
     "Widget {id} updated successfully"
     ```

5. **Network inspector**:
   - F12 → Network tab
   - Filtrează `fetch` requests
   - Observ `GET /api/widgets/{id}/content/` după fiecare schimbare

### Teste specifice pentru DNS Monitor

Dacă glance monitorizează un DNS service (ex: Pi-hole, Technitium):

1. **Simulare DNS offline**:
   ```bash
   # Oprește DNS service
   sudo systemctl stop dnsmasq  # sau service-ul dorit
   ```
   → Dashboard reflectă instant: "Timed Out"

2. **DNS recovery**:
   ```bash
   # Restartează DNS
   sudo systemctl start dnsmasq
   ```
   → Dashboard revine instant: "OK"

### Teste pentru Docker containers

```bash
# Dacă monitorează container status
docker pause <container_name>  # Simulează "offline"
→ Widget afișează Timeout

docker unpause <container_name>  # Simulează "online"
→ Widget revine la OK
```

## Avantaje

1. **Real-time**: actualizări sub 1 secundă după schimbare
2. **Eficient**: transfer minim (doar JSON + widget HTML)
3. **Robust**: fallback la polling pentru clienți care nu suportă SSE
4. **Lifecycle**: cleanup proper la reload config (fără goroutine leaks)
5. **Debounce**: evită spam de actualizări pentru flapping services

## Limitări și future work

1. **Interval fixed**: 15 secunde (ar putea fi configurabil în YAML)
2. **Debounce fixed**: 5 secunde (ar putea fi per-widget configurabil)
3. **Partial updates**: doar widget HTML (ar putea trimite JSON + client side diff pentru optimizare)
4. **No persistence**: evenimentele nu se salvează (client vede doar live stream)
5. **Single hub**: hub-ul este global (ar putea fi per-page pentru scalabilitate)

## Fichiere modificate

- `internal/glance/events.go` - Event hub cu SSE, debounce logic
- `internal/glance/glance.go` - Background worker, context lifecycle, widget endpoint
- `internal/glance/widget-monitor.go` - Status detection și event emission
- `internal/glance/config.go` - lastRenderedContent field pentru page
- `internal/glance/main.go` - Lifecycle management (monitorCancel)
- `internal/glance/templates/widget-base.html` - data-widget-id marker
- `internal/glance/static/js/page.js` - SSE listener și partial DOM updates

## Concluzie

Implementarea live events transformă Glance într-o **aplicație real-time responsive** unde schimbări în serviciile monitorizate se reflectă instant pe dashboard, oferind utilizatorului o experiență modernă și up-to-date fără necesitatea de refresh manual.
