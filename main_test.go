package main

import (
 "encoding/json"
 "fmt"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "strings"
 "testing"
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
