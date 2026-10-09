#!/usr/bin/env python3
"""Archive Arena embed pages and required display resources into Git.

Run explicitly from GitHub Actions or locally with:
    python3 scripts/archive_sites.py

No Arena network access is needed at runtime after this step.
"""
import hashlib
import json
import mimetypes
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urljoin, urlparse
from urllib.request import Request, urlopen

ROOT = Path(__file__).resolve().parent.parent
OUT = ROOT / "snapshots"
ASSETS = OUT / "assets"
SOURCES = {
    1: "https://01a08bd3-c6b2-74bd-a109-f00dda401f46.arena.site/",
    2: "https://01a090b2-8ca0-7927-a790-6347a0e415eb.arena.site/",
    3: "https://01a08fb2-cdd4-71a3-be84-2e210db2a635.arena.site/",
    4: "https://01a090b2-8ca0-7c8e-840e-8d5e40180743.arena.site/",
    5: "https://01a094c4-88a4-7b7c-9e17-e5a3ad505cf2.arena.site/",
    6: "https://01a0953d-925f-7895-915d-17457ab56111.arena.site/",
    7: "https://01a0953d-925f-7b92-a193-25954e884b93.arena.site/",
    8: "https://01a09563-5529-781c-ae88-f7008d8ddb42.arena.site/",
    9: "https://01a09563-5529-74c9-9b86-da9792ecd2a4.arena.site/",
}
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/132.0.0.0 Safari/537.36"
SCRIPT_TAG = re.compile(r"<script\b[^>]*>[\s\S]*?</script\s*>", re.I)
LINK_TAG = re.compile(r"<link\b[^>]*>", re.I)
CSS_URL = re.compile(r"url\(\s*(['\"]?)(https?://[^)'\"\s]+)\1\s*\)", re.I)
ATTR = re.compile(r"""(?i)\b([a-z-]+)\s*=\s*(["'])(.*?)\2""", re.S)
ACTIVE_TAG = re.compile(r"<(?:img|source|video|audio)\b[^>]*>", re.I)
ASSET_CONTENT_TYPES = {
    "font/woff2": ".woff2", "font/woff": ".woff",
    "image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
    "image/svg+xml": ".svg", "image/gif": ".gif", "image/avif": ".avif",
    "text/css": ".css", "application/javascript": ".js",
}
cache = {}


def fetch(url, referer=None, limit=16 * 1024 * 1024):
    parsed = urlparse(url)
    if parsed.scheme != "https" or not parsed.hostname:
        raise ValueError(f"unsupported asset URL: {url!r}")
    headers = {"User-Agent": USER_AGENT, "Accept-Encoding": "identity"}
    if referer:
        headers["Referer"] = referer
    req = Request(url, headers=headers)
    with urlopen(req, timeout=50) as resp:
        data = resp.read(limit + 1)
        if len(data) > limit:
            raise ValueError(f"oversized HTTP response ({limit} bytes limit): {url}")
        return data, resp.headers.get("Content-Type", "").split(";")[0].strip().lower(), resp.url


def file_for(url, referer=None):
    if url in cache:
        return cache[url]
    data, mime, _ = fetch(url, referer)
    ext = ASSET_CONTENT_TYPES.get(mime)
    if not ext:
        ext = Path(urlparse(url).path).suffix.lower()
    if not re.fullmatch(r"\.[a-z0-9]{1,6}", ext or ""):
        raise ValueError(f"unexpected media type for {url}: {mime!r}")
    name = hashlib.sha256(data).hexdigest()[:20] + ext
    path = ASSETS / name
    path.write_bytes(data)
    local = "/assets/" + name
    cache[url] = local
    return local


def inline_font_urls(css, referer):
    def replace(match):
        url = match.group(2)
        return "url(" + file_for(url, referer) + ")"
    return CSS_URL.sub(replace, css)


def strip_tracking_script(match):
    tag = match.group(0)
    opening = tag[:tag.find(">") + 1]
    src = dict((k.lower(), val) for k, _, val in ATTR.findall(opening)).get("src", "")
    if urlparse(src).hostname == "static.cloudflareinsights.com":
        return ""
    if urlparse(src).hostname in {"app.posthog.com", "us.i.posthog.com"}:
        return ""
    return tag


def rewrite_link(match):
    tag = match.group(0)
    attrs = dict((k.lower(), v) for k, _, v in ATTR.findall(tag))
    rel = attrs.get("rel", "").lower()
    href = attrs.get("href", "")
    hostname = urlparse(href).hostname
    if rel == "preconnect" and hostname in {"fonts.googleapis.com", "fonts.gstatic.com"}:
        return ""
    if "stylesheet" in rel and hostname == "fonts.googleapis.com":
        css_data, _, _ = fetch(href)
        css = inline_font_urls(css_data.decode("utf-8"), href)
        return '<style data-local-google-fonts="true">\n' + css + "\n</style>"
    if "stylesheet" in rel and hostname:
        css_data, _, _ = fetch(href)
        css = inline_font_urls(css_data.decode("utf-8"), href)
        return '<style data-local-remote-css="true">\n' + css + "\n</style>"
    if "icon" in rel and href.startswith("/"):
        # Upstream favicons are served by Arena, not the embedded app.
        return ""
    return tag


def rewrite_media_url(match):
    tag = match.group(0)
    def replace_attr(attr):
        key, quote, raw = attr.group(1), attr.group(2), attr.group(3)
        if key.lower() not in {"src", "poster"}:
            return attr.group(0)
        if not raw.startswith("https://"):
            return attr.group(0)
        return key + "=" + quote + file_for(raw) + quote
    return ATTR.sub(replace_attr, tag)


def assert_self_hostable(doc, id):
    if 'id="preview-iframe"' in doc or "Built with Arena" in doc:
        raise RuntimeError(f"variant {id}: Arena wrapper detected instead of embedded app")
    for tag in SCRIPT_TAG.findall(doc):
        op = tag[:tag.find(">") + 1]
        attrs = dict((k.lower(), v) for k, _, v in ATTR.findall(op))
        if attrs.get("src", "").startswith(("http://", "https://", "//")):
            raise RuntimeError(f"variant {id}: external runtime script remains: {attrs['src']}")
    for tag in LINK_TAG.findall(doc):
        attrs = dict((k.lower(), v) for k, _, v in ATTR.findall(tag))
        if "stylesheet" in attrs.get("rel", "").lower() and attrs.get("href", "").startswith(("http://", "https://", "//")):
            raise RuntimeError(f"variant {id}: external stylesheet remains: {attrs['href']}")
    for tag in re.findall(r"<iframe\b[^>]*>", doc, re.I):
        raise RuntimeError(f"variant {id}: iframe remains: {tag[:150]}")
    for tag in ACTIVE_TAG.findall(doc):
        attrs = dict((k.lower(), v) for k, _, v in ATTR.findall(tag))
        for field in ("src", "poster"):
            if attrs.get(field, "").startswith(("http://", "https://", "//")):
                raise RuntimeError(f"variant {id}: external {field} remains: {attrs[field]}")
    if "static.cloudflareinsights.com/beacon" in doc:
        raise RuntimeError(f"variant {id}: Cloudflare RUM beacon not stripped")


def main():
    OUT.mkdir(exist_ok=True)
    ASSETS.mkdir(exist_ok=True)
    info = {}
    for id, base in SOURCES.items():
        print(f"Fetching variant {id}: {base}", flush=True)
        raw, content_type, redirect = fetch(urljoin(base, "?embed=true"), referer=base)
        if content_type not in {"text/html", "application/xhtml+xml"}:
            raise RuntimeError(f"variant {id}: unexpected Content-Type: {content_type}")
        doc = raw.decode("utf-8-sig")
        if len(doc) < 20000:
            raise RuntimeError(f"variant {id}: page unexpectedly small: {len(doc)}")
        doc = SCRIPT_TAG.sub(strip_tracking_script, doc)
        doc = LINK_TAG.sub(rewrite_link, doc)
        doc = ACTIVE_TAG.sub(rewrite_media_url, doc)
        doc = CSS_URL.sub(lambda m: "url(" + file_for(m.group(2)) + ")", doc)
        assert_self_hostable(doc, id)
        out = OUT / f"{id}.html"
        out.write_text(doc, encoding="utf-8")
        info[str(id)] = {
            "source": base,
            "sha256": hashlib.sha256(doc.encode("utf-8")).hexdigest(),
            "bytes": out.stat().st_size,
        }
        print(f"  Saved {out.name}: {out.stat().st_size} bytes", flush=True)
    (OUT / "manifest.json").write_text(
        json.dumps({
            "archived_at_utc": datetime.now(timezone.utc).isoformat(),
            "variants": info,
            "asset_count": len(list(ASSETS.iterdir())),
        }, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Successfully archived {len(info)} pages with {len(list(ASSETS.iterdir()))} local assets")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print("Archive failed:", exc, file=sys.stderr)
        sys.exit(1)
