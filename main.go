package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const previewParam = "__arena_variant"
const variantCount = 9

type app struct{}

const defaultTelegramChatID = "7725563277"

var telegramClient = &http.Client{Timeout: 10 * time.Second}

// A public selection endpoint needs a small abuse limit. Only successful
// notifications count, and a failed Telegram request can be retried.
var telegramRateLimit = struct {
	sync.Mutex
	lastByIP map[string]time.Time
}{lastByIP: make(map[string]time.Time)}

var archivedAssetName = regexp.MustCompile(`^[a-f0-9]{20}\.[a-z0-9]{1,6}$`)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr: ":" + port,
		Handler: app{},
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout: 90 * time.Second,
	}
	log.Printf("serving %d archived websites on :%s (no Arena requests at runtime)", variantCount, port)
	log.Fatal(srv.ListenAndServe())
}

func (app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
		return
	case "/cdn-cgi/rum":
		// Ignore leftover Cloudflare analytics from previously cached pages.
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case "/api/select":
		handleSelection(w, r)
		return
	case "/review":
		n := clampVariant(r.URL.Query().Get("v"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, reviewPage(n))
		return
	}

	if strings.HasPrefix(r.URL.Path, "/assets/") {
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if !archivedAssetName.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(name, ".woff2") {
			w.Header().Set("Content-Type", "font/woff2")
		} else if strings.HasSuffix(name, ".woff") {
			w.Header().Set("Content-Type", "font/woff")
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFile(w, r, filepath.Join("snapshots", "assets", name))
		return
	}

	if id, present := requestedVariant(r); present {
		if id == 0 {
			http.Error(w, "invalid archived variant", http.StatusBadRequest)
			return
		}
		setVariantCookie(w, id)
		serveArchivedVariant(w, r, id)
		return
	}

	if id, rest, ok := parseProxyPath(r.URL.Path); ok {
		// Backwards-compatible links from the old proxy never need Arena.
		query := r.URL.Query()
		query.Set(previewParam, strconv.Itoa(id))
		dest := rest + "?" + query.Encode()
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
		return
	}

	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, homePage())
		return
	}

	// SPA history navigations can request /about, /courses, etc.
	// Serve the selected saved page again without any upstream fetch.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		if id, ok := cookieVariant(r); ok {
			serveArchivedVariant(w, r, id)
			return
		}
	}
	// All unarchived assets fail closed, instead of contacting arena.site.
	http.NotFound(w, r)
}

// handleSelection receives a variant only after the user explicitly clicks
// "Выбрать вариант". The bot token is never sent to the browser.
func handleSelection(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Reject cross-site browser calls to avoid unsolicited notifications.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" ||
		(r.Header.Get("Origin") != "" && !sameRequestOrigin(r)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return
	}
	var payload struct {
		Variant int `json:"variant"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil || payload.Variant < 1 || payload.Variant > variantCount {
		http.Error(w, "invalid variant", http.StatusBadRequest)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(os.Getenv("token_bot")) == "" {
		log.Printf("telegram notification unavailable: Render variable token_bot is not configured")
		http.Error(w, "Telegram notifications are not configured", http.StatusServiceUnavailable)
		return
	}

	ip := clientIP(r)
	telegramRateLimit.Lock()
	if last, ok := telegramRateLimit.lastByIP[ip]; ok && time.Since(last) < 5*time.Second {
		telegramRateLimit.Unlock()
		http.Error(w, "please wait before selecting again", http.StatusTooManyRequests)
		return
	}
	// Keep the lock during the send so parallel clicks from one user
	// cannot bypass the limit. Notifications take at most 10 seconds.
	err := notifyTelegram(r, payload.Variant)
	if err == nil {
		telegramRateLimit.lastByIP[ip] = time.Now()
	}
	telegramRateLimit.Unlock()
	if err != nil {
		log.Printf("telegram notification failed: %v", err)
		http.Error(w, "Telegram message could not be delivered", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func sameRequestOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// Render may be behind a reverse proxy; use the request Host, which
	// preserves the public hostname in Go's HTTP server.
	return origin == "https://"+r.Host || origin == "http://"+r.Host
}

func clientIP(r *http.Request) string {
	// RemoteAddr is provided by the actual server. Do not trust arbitrary
	// client-supplied X-Forwarded-For, which attackers could spoof.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func notifyTelegram(r *http.Request, variant int) error {
	token := strings.TrimSpace(os.Getenv("token_bot"))
	chatID := strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	if chatID == "" {
		chatID = defaultTelegramChatID
	}
	msg := fmt.Sprintf("✅ Выбран вариант сайта: %02d из %02d", variant, variantCount)
	payload, err := json.Marshal(map[string]string{
		"chat_id": chatID,
		"text": msg,
	})
	if err != nil {
		return errors.New("encode Telegram message failed")
	}
	endpoint := "https://api.telegram.org/bot"+token+"/sendMessage"
	if override := os.Getenv("TELEGRAM_API_BASE_URL"); override != "" {
		// A test-only override is used by regression tests, never required on Render.
		endpoint = strings.TrimRight(override, "/") + "/bot" + token + "/sendMessage"
	}
	out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return errors.New("create Telegram request failed")
	}
	out.Header.Set("Content-Type", "application/json")
	response, err := telegramClient.Do(out)
	if err != nil {
		// Do not log errors containing the request URL: it includes the bot token.
		return errors.New("Telegram API connection failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return errors.New("Telegram API response could not be read")
	}
	var result struct { Ok bool `json:"ok"` }
	if response.StatusCode != http.StatusOK || json.Unmarshal(body, &result) != nil || !result.Ok {
		return fmt.Errorf("Telegram API rejected message (HTTP %d)", response.StatusCode)
	}
	return nil
}

func serveArchivedVariant(w http.ResponseWriter, r *http.Request, id int) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := filepath.Join("snapshots", strconv.Itoa(id)+".html")
	file, err := os.Open(name)
	if err != nil {
		log.Printf("archived variant %d missing: %v", id, err)
		http.Error(w, "Сохранённая копия временно недоступна", http.StatusServiceUnavailable)
		return
	}
	file.Close()
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeFile(w, r, name)
}

func requestedVariant(r *http.Request) (int, bool) {
	values, present := r.URL.Query()[previewParam]
	if !present {
		return 0, false
	}
	if len(values) != 1 {
		return 0, true
	}
	n, err := strconv.Atoi(values[0])
	if err != nil || n < 1 || n > variantCount {
		return 0, true
	}
	return n, true
}

func clampVariant(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > variantCount {
		return 1
	}
	return n
}

func cookieVariant(r *http.Request) (int, bool) {
	c, err := r.Cookie("arena_variant")
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(c.Value)
	return n, err == nil && n >= 1 && n <= variantCount
}

func setVariantCookie(w http.ResponseWriter, id int) {
	http.SetCookie(w, &http.Cookie{
		Name: "arena_variant",
		Value: strconv.Itoa(id),
		Path: "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func parseProxyPath(path string) (int, string, bool) {
	if !strings.HasPrefix(path, "/p/") {
		return 0, "", false
	}
	tail := strings.TrimPrefix(path, "/p/")
	parts := strings.SplitN(tail, "/", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 || n > variantCount {
		return 0, "", false
	}
	rest := "/"
	if len(parts) == 2 {
		rest += parts[1]
	}
	return n, rest, true
}

func homePage() string {
	return `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Выбор варианта</title><style>
*{box-sizing:border-box}body{margin:0;background:#0b0c0e;color:#f5f5f5;font:15px/1.5 Inter,system-ui,sans-serif}main{max-width:1050px;margin:auto;padding:48px 20px}h1{font-size:clamp(36px,7vw,72px);line-height:.95;letter-spacing:-.05em;margin:10px 0 18px}.sub{color:#999;max-width:560px;margin-bottom:30px}.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:10px}.card{min-height:150px;padding:20px;border-radius:18px;border:1px solid #292c31;background:#121417;text-decoration:none;color:#fff;display:flex;flex-direction:column;justify-content:space-between;transition:.15s}.card:hover{transform:translateY(-2px);background:#191c20;border-color:#555}.n{font-size:48px;font-weight:800;color:#424750;letter-spacing:-.06em}.t{font-weight:700}.note{color:#737984;margin-top:24px;font-size:13px}@media(max-width:650px){main{padding:28px 14px}.grid{grid-template-columns:repeat(2,1fr)}.card{min-height:125px}}
</style></head><body><main><small>ПРЕДПРОСМОТР</small><h1>Выберите вариант сайта</h1><div class="sub">Открывай варианты по очереди. Все варианты сохранены в GitHub и выдаются с Render. Для просмотра не требуется открывать arena.site.</div><div class="grid">` + cards() + `</div><div class="note">9 сохранённых вариантов · без запросов к Arena при просмотре</div></main></body></html>`
}

func cards() string {
	var b strings.Builder
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&b, `<a class="card" href="/review?v=%d"><div class="n">%02d</div><div class="t">Вариант %d →</div></a>`, i, i, i)
	}
	return b.String()
}

func reviewPage(initial int) string {
	return fmt.Sprintf(`<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Вариант %d</title><style>
*{box-sizing:border-box}html,body{height:100%%;margin:0;background:#0c0d0f;font-family:Inter,system-ui,sans-serif}.app{height:100%%;display:grid;grid-template-rows:56px 1fr}.bar{display:flex;align-items:center;gap:8px;padding:8px;background:#121418;border-bottom:1px solid #2a2d32}.back,.tab,.pick{height:38px;border:0;border-radius:10px;font-weight:700}.back{width:38px;display:grid;place-items:center;background:#24272d;color:#fff;text-decoration:none}.tabs{display:flex;gap:5px;overflow:auto}.tab{min-width:38px;background:transparent;color:#838995;cursor:pointer}.tab.active{background:#f3f4f6;color:#090a0b}.space{flex:1}.pick{background:#f3f4f6;color:#090a0b;padding:0 13px;white-space:nowrap;cursor:pointer}.frame{position:relative;background:#fff}.frame iframe{width:100%%;height:100%%;border:0}.load{position:absolute;inset:0;display:grid;place-items:center;background:#101216;color:#8d929b;pointer-events:none}.load.hide{display:none}.toast{position:fixed;left:50%%;bottom:22px;transform:translateX(-50%%);background:#15181d;color:#fff;border:1px solid #373b43;padding:12px 16px;border-radius:12px;display:none;z-index:4}.toast.show{display:block}@media(max-width:650px){.pick{font-size:0}.pick:after{content:'✓';font-size:16px}.tabs{max-width:calc(100vw - 105px)}}
</style></head><body><div class="app"><header class="bar"><a class="back" href="/">←</a><div class="tabs" id="tabs"></div><div class="space"></div><button class="pick" id="pick">Выбрать вариант</button></header><main class="frame"><div class="load" id="load">Загрузка…</div><iframe id="site" referrerpolicy="no-referrer"></iframe></main></div><div class="toast" id="toast"></div><script>
let n=%d;const f=document.getElementById('site'),load=document.getElementById('load'),toast=document.getElementById('toast');
function openV(x){n=x;load.classList.remove('hide');f.src='/?__arena_variant='+x;history.replaceState(null,'','/review?v='+x);document.querySelectorAll('.tab').forEach((e,i)=>e.classList.toggle('active',i+1===x))}
for(let i=1;i<=9;i++){const b=document.createElement('button');b.className='tab';b.textContent=String(i).padStart(2,'0');b.onclick=()=>openV(i);tabs.appendChild(b)}
f.onload=()=>load.classList.add('hide');openV(n);
pick.onclick=async()=>{const selected=n;pick.disabled=true;toast.textContent='Отправка выбранного варианта…';toast.classList.add('show');try{const res=await fetch('/api/select',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({variant:selected})});if(!res.ok)throw Error('HTTP '+res.status);localStorage.setItem('chosenVariant',String(selected));toast.textContent='✓ Вариант '+selected+' отправлен в Telegram'}catch(e){toast.textContent='Не удалось отправить в Telegram. Попробуй ещё раз.'}finally{pick.disabled=false;setTimeout(()=>toast.classList.remove('show'),3500)}};
</script></body></html>`, initial, initial)
}
