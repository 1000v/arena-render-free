package main

import (
 "net/http"
 "net/http/httputil"
 "net/http/httptest"
 "net/url"
 "io"
 "strings"
 "testing"
)

func TestVariantSelection(t *testing.T) {
 for _, tc := range []struct{ raw string; want int; present bool }{
  {"/?__arena_variant=1",1,true},
  {"/?__arena_variant=9",9,true},
  {"/?__arena_variant=0",0,true},
  {"/?__arena_variant=10",0,true},
  {"/?__arena_variant=1&__arena_variant=2",0,true},
  {"/",0,false},
 } {
  req:=httptest.NewRequest(http.MethodGet,tc.raw,nil)
  got,present:=requestedVariant(req)
  if got!=tc.want || present!=tc.present { t.Errorf("%s got (%d,%v), want (%d,%v)",tc.raw,got,present,tc.want,tc.present) }
 }
}

func TestLegacyPreviewRedirect(t *testing.T) {
 a:=&app{proxies:map[int]*httputil.ReverseProxy{}}
 rec:=httptest.NewRecorder()
 a.ServeHTTP(rec,httptest.NewRequest(http.MethodGet,"/p/3/some/path?foo=bar",nil))
 if rec.Code!=http.StatusTemporaryRedirect { t.Fatalf("status: %d",rec.Code) }
 loc:=rec.Header().Get("Location")
 u,err:=url.Parse(loc)
 if err!=nil || u.Path!="/some/path" || u.Query().Get("foo")!="bar" || u.Query().Get(previewParam)!="3" { t.Errorf("location: %s",loc) }
}

func TestRootHTMLNavigation(t *testing.T) {
 target,_:=url.Parse("https://example.arena.site")
 input:=[]byte("<a href=\"/about\">About</a><script src=\"/assets/app.js\"></script>")
 result:=string(rewriteBody(input,target,2,"text/html"))
 if !strings.Contains(result,"/about?__arena_variant=2") { t.Errorf("navigation URL not selected: %s",result) }
 if !strings.Contains(result,"src=\"/assets/app.js\"") { t.Errorf("asset URL modified: %s",result) }
 if strings.Contains(result,"/p/2") { t.Errorf("old prefix retained: %s",result) }
}

func TestRedirect(t *testing.T) {
 target,_:=url.Parse("https://example.arena.site")
 if got:=rewriteLocation("/dashboard?x=1",target,4);got!="/dashboard?__arena_variant=4&x=1" { t.Errorf("redirect %q",got) }
 if got:=rewriteLocation("https://external.example/path",target,4);got!="https://external.example/path" { t.Errorf("external redirect %q",got) }
}

func TestEmbeddedArenaLoadsWithoutNestedWrapper(t *testing.T) {
 var upstreamPath, upstreamReferer string
 upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  upstreamPath = r.URL.String()
  upstreamReferer = r.Referer()
  w.Header().Set("Set-Cookie", "__cf_bm=placeholder; Domain=arena.site; Path=/; HttpOnly")
  w.Header().Set("Permissions-Policy", "accelerometer=()")
  w.Header().Set("Content-Type", "text/html; charset=utf-8")
  if r.URL.Query().Get("embed")!="true" || r.Referer()=="" {
   http.Redirect(w,r,"/",http.StatusFound)
   return
  }
  io.WriteString(w,"<html><body>Embedded app, not preview wrapper</body></html>")
 }))
 defer upstream.Close()
 proxy,err:=newProxy(variant{ID:1,URL:upstream.URL+"/"})
 if err!=nil { t.Fatal(err) }
 a:=&app{proxies:map[int]*httputil.ReverseProxy{1:proxy}}
 res:=httptest.NewRecorder()
 a.ServeHTTP(res,httptest.NewRequest(http.MethodGet,"/?__arena_variant=1",nil))
 if res.Code!=http.StatusOK { t.Fatalf("HTTP %d body=%s",res.Code,res.Body.String()) }
 if !strings.Contains(res.Body.String(),"Embedded app, not preview wrapper") { t.Fatalf("wrong HTML: %s",res.Body.String()) }
 if upstreamPath!="/?embed=true" { t.Errorf("unexpected upstream URL %q",upstreamPath) }
 if upstreamReferer!=upstream.URL+"/" { t.Errorf("upstream Referer=%q",upstreamReferer) }
 if h:=res.Header().Get("Permissions-Policy");h!="" { t.Errorf("unsupported policy leaked: %s",h) }
 for _,cookie:=range res.Header().Values("Set-Cookie") {
  if strings.Contains(strings.ToLower(cookie),"domain=arena.site") { t.Errorf("invalid cookie domain: %q",cookie) }
 }
}

func TestReviewUsesEmbedMode(t *testing.T) {
 if !strings.Contains(reviewPage(2),"f.src='/?embed=true&__arena_variant='+x") { t.Fatal("review iframe does not open embedded mode") }
}
