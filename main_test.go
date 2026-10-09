package main

import (
 "net/http"
 "net/http/httputil"
 "net/http/httptest"
 "net/url"
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
