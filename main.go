package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type variant struct {
	ID  int
	URL string
}

var variants = []variant{
	{1, "https://01a08bd3-c6b2-74bd-a109-f00dda401f46.arena.site/"},
	{2, "https://01a090b2-8ca0-7927-a790-6347a0e415eb.arena.site/"},
	{3, "https://01a08fb2-cdd4-71a3-be84-2e210db2a635.arena.site/"},
	{4, "https://01a090b2-8ca0-7c8e-840e-8d5e40180743.arena.site/"},
	{5, "https://01a094c4-88a4-7b7c-9e17-e5a3ad505cf2.arena.site/"},
	{6, "https://01a0953d-925f-7895-915d-17457ab56111.arena.site/"},
	{7, "https://01a0953d-925f-7b92-a193-25954e884b93.arena.site/"},
	{8, "https://01a09563-5529-781c-ae88-f7008d8ddb42.arena.site/"},
	{9, "https://01a09563-5529-74c9-9b86-da9792ecd2a4.arena.site/"},
}

type app struct {
	proxies map[int]*httputil.ReverseProxy
}

func main() {
	a := &app{proxies: map[int]*httputil.ReverseProxy{}}
	for _, v := range variants {
		p, err := newProxy(v)
		if err != nil {
			log.Fatal(err)
		}
		a.proxies[v.ID] = p
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	s := &http.Server{
		Addr: ":" + port, Handler: a,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	log.Printf("listening on :%s", port)
	log.Fatal(s.ListenAndServe())
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok")
		return
	}
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, homePage())
		return
	}
	if r.URL.Path == "/review" {
		n := clampVariant(r.URL.Query().Get("v"))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, reviewPage(n))
		return
	}

	// Main proxy path: /p/1/..., /p/2/..., etc.
	if id, rest, ok := parseProxyPath(r.URL.Path); ok {
		http.SetCookie(w, &http.Cookie{
			Name: "arena_variant", Value: strconv.Itoa(id), Path: "/",
			SameSite: http.SameSiteLaxMode,
		})
		r.URL.Path = rest
		if r.URL.RawPath != "" {
			r.URL.RawPath = ""
		}
		a.proxies[id].ServeHTTP(w, r)
		return
	}

	// Fallback for root-relative assets such as /assets/app.js.
	// The last opened preview sets arena_variant, so these requests still
	// go to the correct Arena version.
	if c, err := r.Cookie("arena_variant"); err == nil {
		id := clampVariant(c.Value)
		if _, reserved := map[string]bool{"/": true, "/review": true, "/healthz": true}[r.URL.Path]; !reserved {
			a.proxies[id].ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

func parseProxyPath(path string) (int, string, bool) {
	if !strings.HasPrefix(path, "/p/") {
		return 0, "", false
	}
	tail := strings.TrimPrefix(path, "/p/")
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) == 0 {
		return 0, "", false
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 || n > len(variants) {
		return 0, "", false
	}
	rest := "/"
	if len(parts) == 2 {
		rest += parts[1]
	}
	return n, rest, true
}

func clampVariant(s string) int {
	n, _ := strconv.Atoi(s)
	if n < 1 || n > len(variants) {
		return 1
	}
	return n
}

func newProxy(v variant) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(v.URL)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(target)
	orig := p.Director
	p.Director = func(req *http.Request) {
		orig(req)
		req.Host = target.Host
		req.Header.Set("Host", target.Host)
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Del("Origin")
		req.Header.Del("Referer")
	}
	p.ModifyResponse = func(resp *http.Response) error {
		for _, h := range []string{
			"Content-Security-Policy", "Content-Security-Policy-Report-Only",
			"X-Frame-Options", "Cross-Origin-Opener-Policy",
			"Cross-Origin-Embedder-Policy", "Cross-Origin-Resource-Policy",
		} {
			resp.Header.Del(h)
		}

		prefix := fmt.Sprintf("/p/%d", v.ID)
		if loc := resp.Header.Get("Location"); loc != "" {
			resp.Header.Set("Location", rewriteLocation(loc, target, prefix))
		}
		if resp.Body == nil || !rewritable(resp.Header.Get("Content-Type")) {
			return nil
		}

		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return err
		}
		resp.Body.Close()
		b = rewriteBody(b, target, prefix, resp.Header.Get("Content-Type"))
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("ETag")
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy v%d: %v", v.ID, err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `<meta charset="utf-8"><style>body{font-family:system-ui;background:#111;color:#eee;padding:40px}</style><h2>Вариант %d временно не загрузился</h2><p>Обнови страницу через несколько секунд.</p>`, v.ID)
	}
	return p, nil
}

var attrRoot = regexp.MustCompile(`(?i)(href|src|action|poster)=(['"])/([^/])`)
var cssRoot = regexp.MustCompile(`(?i)url\((['"]?)/([^/])`)

func rewriteBody(b []byte, target *url.URL, prefix, ct string) []byte {
	s := string(b)
	upstream := target.Scheme + "://" + target.Host
	// Absolute links back to this exact Arena host.
	s = strings.ReplaceAll(s, upstream, prefix)
	s = strings.ReplaceAll(s, "//"+target.Host, prefix)
	s = strings.ReplaceAll(s, strings.ReplaceAll(upstream, "/", `\/`), strings.ReplaceAll(prefix, "/", `\/`))

	lower := strings.ToLower(ct)
	if strings.Contains(lower, "text/html") {
		// Root-relative resources.
		s = attrRoot.ReplaceAllString(s, `${1}=${2}`+prefix+`/${3}`)
		// Exact root links (href="/", action="/", etc.).
		for _, a := range []string{"href", "src", "action", "poster"} {
			for _, q := range []string{`"`, `'`} {
				s = strings.ReplaceAll(s, a+`=`+q+`/`+q, a+`=`+q+prefix+`/`+q)
			}
		}
		// Relative links resolve inside the selected proxy prefix.
		base := `<base href="` + prefix + `/">`
		if i := strings.Index(strings.ToLower(s), "<head>"); i >= 0 {
			i += len("<head>")
			s = s[:i] + base + s[i:]
		}
	}
	if strings.Contains(lower, "text/css") {
		s = cssRoot.ReplaceAllString(s, `url(${1}`+prefix+`/${2}`)
	}
	return []byte(s)
}

func rewriteLocation(loc string, target *url.URL, prefix string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() && strings.EqualFold(u.Host, target.Host) {
		out := prefix + u.EscapedPath()
		if u.RawQuery != "" {
			out += "?" + u.RawQuery
		}
		if u.Fragment != "" {
			out += "#" + u.Fragment
		}
		return out
	}
	if strings.HasPrefix(loc, "/") {
		return prefix + loc
	}
	return loc
}

func rewritable(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "text/html") || strings.Contains(ct, "text/css") ||
		strings.Contains(ct, "javascript") || strings.Contains(ct, "application/json") ||
		strings.Contains(ct, "image/svg+xml") || strings.Contains(ct, "text/plain")
}

func homePage() string {
	return `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Выбор варианта</title><style>
*{box-sizing:border-box}body{margin:0;background:#0b0c0e;color:#f5f5f5;font:15px/1.5 Inter,system-ui,sans-serif}main{max-width:1050px;margin:auto;padding:48px 20px}h1{font-size:clamp(36px,7vw,72px);line-height:.95;letter-spacing:-.05em;margin:10px 0 18px}.sub{color:#999;max-width:560px;margin-bottom:30px}.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:10px}.card{min-height:150px;padding:20px;border-radius:18px;border:1px solid #292c31;background:#121417;text-decoration:none;color:#fff;display:flex;flex-direction:column;justify-content:space-between;transition:.15s}.card:hover{transform:translateY(-2px);background:#191c20;border-color:#555}.n{font-size:48px;font-weight:800;color:#424750;letter-spacing:-.06em}.t{font-weight:700}.note{color:#737984;margin-top:24px;font-size:13px}@media(max-width:650px){main{padding:28px 14px}.grid{grid-template-columns:repeat(2,1fr)}.card{min-height:125px}}
</style></head><body><main><small>ПРЕДПРОСМОТР</small><h1>Выберите вариант сайта</h1><div class="sub">Открывай варианты по очереди. Всё грузится через эту ссылку — arena.site заказчику открывать не нужно.</div><div class="grid">` + cards() + `</div><div class="note">9 вариантов · один бесплатный сервер · без домена</div></main></body></html>`
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
function openV(x){n=x;load.classList.remove('hide');f.src='/p/'+x+'/';history.replaceState(null,'','/review?v='+x);document.querySelectorAll('.tab').forEach((e,i)=>e.classList.toggle('active',i+1===x))}
for(let i=1;i<=9;i++){const b=document.createElement('button');b.className='tab';b.textContent=String(i).padStart(2,'0');b.onclick=()=>openV(i);tabs.appendChild(b)}
f.onload=()=>load.classList.add('hide');openV(n);
pick.onclick=async()=>{const text='Выбран вариант '+n;try{await navigator.clipboard.writeText(text)}catch(e){}localStorage.setItem('chosenVariant',String(n));toast.textContent='✓ '+text+' — номер скопирован';toast.classList.add('show');setTimeout(()=>toast.classList.remove('show'),2500)};
</script></body></html>`, initial, initial)
}
