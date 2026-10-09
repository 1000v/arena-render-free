package main

import (
 "encoding/json"
 "io"
 "fmt"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "strings"
 "testing"
 "time"
)

func request(path string) *httptest.ResponseRecorder {
 w:=httptest.NewRecorder()
 app{}.ServeHTTP(w,httptest.NewRequest(http.MethodGet,path,nil))
 return w
}

func TestRootAndReview(t *testing.T) {
 home:=request("/")
 if home.Code!=http.StatusOK || !strings.Contains(home.Body.String(),"Выберите вариант сайта") { t.Fatalf("homepage: HTTP %d",home.Code) }
 review:=request("/review?v=3")
 if review.Code!=http.StatusOK || !strings.Contains(review.Body.String(),"f.src='/?__arena_variant='+x") {t.Fatalf("review: HTTP %d",review.Code)}
}

func TestAllNineVariantsAreArchivedAndIndependentlyServed(t *testing.T) {
 for i:=1;i<=variantCount;i++ {
  i:=i
  t.Run(fmt.Sprint(i),func(t *testing.T){
   rec:=request(fmt.Sprintf("/?__arena_variant=%d",i))
   if rec.Code!=http.StatusOK { t.Fatalf("variant %d HTTP %d: %s",i,rec.Code,rec.Body.String()) }
   html:=rec.Body.String()
   if len(html)<20000 || !strings.Contains(strings.ToLower(html),"<html") {t.Fatalf("variant %d invalid HTML, size=%d",i,len(html))}
   if strings.Contains(html, "id=\"preview-iframe\"") {t.Fatalf("variant %d contains Arena wrapper",i)}
   if strings.Contains(html,"static.cloudflareinsights.com/beacon") {t.Fatalf("variant %d contains Cloudflare RUM",i)}
   if !strings.Contains(rec.Header().Get("Content-Type"),"text/html") {t.Fatalf("variant %d response is not HTML",i)}
   cookieFound:=false
   for _,c:=range rec.Result().Cookies() {if c.Name=="arena_variant" && c.Value==fmt.Sprint(i) {cookieFound=true}}
   if !cookieFound {t.Fatal("selection cookie missing")}
  })
 }
}

func TestManifestAndAssetsAreBundled(t *testing.T){
 data,err:=os.ReadFile("snapshots/manifest.json")
 if err!=nil {t.Fatal(err)}
 var info struct {
  Variants map[string]any `json:"variants"`
  AssetCount int `json:"asset_count"`
 }
 if err:=json.Unmarshal(data,&info);err!=nil {t.Fatal(err)}
 if len(info.Variants)!=variantCount {t.Fatalf("expected %d archived pages, got %d",variantCount,len(info.Variants))}
 if info.AssetCount<1 {t.Fatal("expected local fonts/assets")}
 entries,err:=os.ReadDir("snapshots/assets")
 if err!=nil {t.Fatal(err)}
 if len(entries)!=info.AssetCount {t.Fatalf("manifest asset_count=%d, actual=%d",info.AssetCount,len(entries))}
 for _,entry:=range entries{
  path:=filepath.Join("snapshots","assets",entry.Name())
  if !entry.Type().IsRegular(){t.Fatalf("not a regular asset: %s",entry.Name())}
  if _,err:=os.Stat(path);err!=nil {t.Fatal(err)}
  rec:=request("/assets/"+entry.Name())
  if rec.Code!=http.StatusOK {t.Fatalf("asset %s HTTP %d",entry.Name(),rec.Code)}
 }
}

func TestNoNetworkFallback(t *testing.T){
 for _,path:=range []string{"/not-existing.js","/assets/not-a-hash.js","/cdn-cgi/challenge-platform/random","/favicon.ico"} {
  rec:=request(path)
  if rec.Code!=http.StatusNotFound {t.Errorf("%s returned HTTP %d; expected local 404",path,rec.Code)}
 }
}

func TestOldURLsRedirectToLocalSnapshots(t *testing.T) {
 rec:=request("/p/2/some/page?foo=bar")
 if rec.Code!=http.StatusTemporaryRedirect {t.Fatalf("got HTTP %d",rec.Code)}
 loc:=rec.Header().Get("Location")
 if loc!="/some/page?__arena_variant=2&foo=bar" {t.Fatalf("unexpected location: %s",loc)}
}

func TestInvalidVariant(t *testing.T){
 for _,p:=range []string{"/?__arena_variant=0","/?__arena_variant=10","/?__arena_variant=1&__arena_variant=2"}{
  if rec:=request(p);rec.Code!=http.StatusBadRequest {t.Errorf("%s status=%d",p,rec.Code)}
 }
}

func TestDiscardLegacyRUM(t *testing.T){
 for _,p:=range []string{"/cdn-cgi/rum","/cdn-cgi/rum?anything=1"} {
  r:=httptest.NewRequest(http.MethodPost,p,strings.NewReader("old cloudflare payload"))
  rec:=httptest.NewRecorder()
  app{}.ServeHTTP(rec,r)
  if rec.Code!=http.StatusNoContent || rec.Body.Len()!=0 {t.Fatalf("POST %s: HTTP %d (%q)",p,rec.Code,rec.Body.String())}
 }
 if rec:=request("/cdn-cgi/rum");rec.Code!=http.StatusMethodNotAllowed {t.Fatalf("GET RUM: HTTP %d",rec.Code)}
}

func TestSPAHistoricalNavigation(t *testing.T){
 initial:=httptest.NewRecorder()
 app{}.ServeHTTP(initial,httptest.NewRequest(http.MethodGet,"/?__arena_variant=4",nil))
 r:=httptest.NewRequest(http.MethodGet,"/courses",nil)
 for _,c:=range initial.Result().Cookies(){r.AddCookie(c)}
 r.Header.Set("Accept","text/html,application/xhtml+xml")
 rec:=httptest.NewRecorder()
 app{}.ServeHTTP(rec,r)
 if rec.Code!=http.StatusOK || rec.Body.Len()<20000 {t.Fatalf("history route: HTTP %d",rec.Code)}
}

func TestTelegramSelectionSendsOnlyChosenVariant(t *testing.T) {
 var calls int
 var gotPath string
 var gotChat, gotText string
 telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  calls++
  gotPath = r.URL.Path
  if r.Method!=http.MethodPost {t.Errorf("upstream method: %s",r.Method)}
  if r.Header.Get("Content-Type")!="application/json" {t.Errorf("upstream Content-Type: %s",r.Header.Get("Content-Type"))}
  var data struct { ChatID string `json:"chat_id"`; Text string `json:"text"` }
  if err:=json.NewDecoder(r.Body).Decode(&data);err!=nil {t.Errorf("Telegram JSON: %v",err)}
  gotChat,gotText=data.ChatID,data.Text
  w.Header().Set("Content-Type","application/json")
  io.WriteString(w,`{"ok":true,"result":{"message_id":10}}`)
 }))
 defer telegram.Close()
 t.Setenv("token_bot","test_token_not_real")
 t.Setenv("TELEGRAM_CHAT_ID","7725563277")
 t.Setenv("TELEGRAM_API_BASE_URL",telegram.URL)
 telegramRateLimit.Lock(); telegramRateLimit.lastByIP=make(map[string]time.Time);telegramRateLimit.Unlock()
 req:=httptest.NewRequest(http.MethodPost,"https://arena-variants.onrender.com/api/select",strings.NewReader(`{"variant":7}`))
 req.Header.Set("Content-Type","application/json")
 req.Header.Set("Origin","https://arena-variants.onrender.com")
 req.RemoteAddr="192.0.2.71:34567"
 rec:=httptest.NewRecorder()
 app{}.ServeHTTP(rec,req)
 if rec.Code!=http.StatusOK {t.Fatalf("selection HTTP %d: %s",rec.Code,rec.Body.String())}
 if calls!=1 {t.Fatalf("expected one Telegram call; got %d",calls)}
 if gotPath!="/bottest_token_not_real/sendMessage" {t.Errorf("endpoint=%s",gotPath)}
 if gotChat!="7725563277" {t.Errorf("chat=%s",gotChat)}
 if !strings.Contains(gotText,"07") {t.Errorf("text=%s",gotText)}
 if !strings.Contains(reviewPage(4),"fetch('/api/select'") {t.Fatal("button does not submit selection to backend")}
 if strings.Contains(reviewPage(4),"test_token_not_real") {t.Fatal("bot token leaked to browser")}
 // Retrying immediately from the same IP must not send a duplicate.
 rec2:=httptest.NewRecorder()
 app{}.ServeHTTP(rec2,req.Clone(req.Context()))
 if rec2.Code!=http.StatusTooManyRequests || calls!=1 {t.Fatalf("duplicate status=%d, calls=%d",rec2.Code,calls)}
}

func TestSelectionValidationAndErrors(t *testing.T) {
 t.Setenv("token_bot","test_token_not_real")
 t.Setenv("TELEGRAM_API_BASE_URL","http://127.0.0.1:1")
 cases:=[]struct{name,method,body,contentType,origin string;code int}{
  {"method","GET","", "", "",http.StatusMethodNotAllowed},
  {"missing-type","POST",`{"variant":2}`,"text/plain","",http.StatusUnsupportedMediaType},
  {"invalid-zero","POST",`{"variant":0}`,"application/json","",http.StatusBadRequest},
  {"invalid-ten","POST",`{"variant":10}`,"application/json","",http.StatusBadRequest},
  {"unknown-fields","POST",`{"variant":2,"text":"injection"}`,"application/json","",http.StatusBadRequest},
  {"trailing-json","POST",`{"variant":2}{"variant":3}`,"application/json","",http.StatusBadRequest},
  {"wrong-origin","POST",`{"variant":2}`,"application/json","https://evil.example",http.StatusForbidden},
 }
 for _,tc:=range cases {
  t.Run(tc.name,func(t *testing.T){
   req:=httptest.NewRequest(tc.method,"https://arena-variants.onrender.com/api/select",strings.NewReader(tc.body))
   if tc.contentType!="" {req.Header.Set("Content-Type",tc.contentType)}
   if tc.origin!="" {req.Header.Set("Origin",tc.origin)}
   rec:=httptest.NewRecorder()
   app{}.ServeHTTP(rec,req)
   if rec.Code!=tc.code {t.Fatalf("got HTTP %d wanted %d",rec.Code,tc.code)}
  })
 }
}

func TestTelegramDownstreamErrorNeverReturnsSuccess(t *testing.T) {
 telegram:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  w.Header().Set("Content-Type","application/json")
  w.WriteHeader(http.StatusForbidden)
  io.WriteString(w,`{"ok":false,"description":"Forbidden"}`)
 }))
 defer telegram.Close()
 t.Setenv("token_bot","dummy_secret")
 t.Setenv("TELEGRAM_API_BASE_URL",telegram.URL)
 telegramRateLimit.Lock();telegramRateLimit.lastByIP=make(map[string]time.Time);telegramRateLimit.Unlock()
 req:=httptest.NewRequest(http.MethodPost,"https://arena-variants.onrender.com/api/select",strings.NewReader(`{"variant":3}`))
 req.Header.Set("Content-Type","application/json")
 req.RemoteAddr="192.0.2.73:23456"
 rec:=httptest.NewRecorder()
 app{}.ServeHTTP(rec,req)
 if rec.Code!=http.StatusBadGateway {t.Fatalf("Telegram rejection returned HTTP %d instead of 502",rec.Code)}
 if strings.Contains(rec.Body.String(),"dummy_secret") {t.Fatal("sensitive token leaked")}
}

func TestSelectionRequiresConfiguredBotToken(t *testing.T) {
 t.Setenv("token_bot","")
 req:=httptest.NewRequest(http.MethodPost,"https://arena-variants.onrender.com/api/select",strings.NewReader(`{"variant":5}`))
 req.Header.Set("Content-Type","application/json")
 rec:=httptest.NewRecorder()
 app{}.ServeHTTP(rec,req)
 if rec.Code!=http.StatusServiceUnavailable {t.Fatalf("missing token: got HTTP %d",rec.Code)}
}
