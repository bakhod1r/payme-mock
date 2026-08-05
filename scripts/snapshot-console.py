#!/usr/bin/env python3
"""Freeze the running console into a browsable static snapshot for GitHub Pages.

Usage: bring the stand up, seed it, drive some payments through it, then

    python3 scripts/snapshot-console.py docs/demo


The console serves fully inline CSS and no assets, so a fetched page is already
self-contained. What has to change is the linking: every href is an absolute
server path, and every mutation is a form POST that a static host cannot
answer. Links become sibling .html files, and forms become inert.
"""

import hashlib
import html
import json
import os
import re
import sys
import urllib.parse
import urllib.request

BASE = os.environ.get("CONSOLE_URL", "http://127.0.0.1:8080")
OUT = sys.argv[1] if len(sys.argv) > 1 else "docs/demo"

# Where the crawl starts. Detail pages are reached by following links from here.
SEEDS = ["/dashboard", "/sandboxes", "/payments", "/cards", "/rules", "/traffic"]

# Filters combine, so the reachable set is much larger than the screen count.
# Only one filter deep is captured: a filtered screen's own filter links are not
# followed, so the snapshot shows what each filter does without multiplying out
# every pairing of them. This is the bound that keeps the published site a few
# megabytes rather than a few hundred.
MAX_PAGES = 200

BANNER = """
<div style="position:sticky;top:0;z-index:999;background:#1d2b3a;color:#cfe3ff;
     border-bottom:1px solid #2f4böd;padding:9px 24px;font:13px/1.5 ui-sans-serif,
     -apple-system,'Segoe UI',Roboto,sans-serif;text-align:center">
  <strong>Static snapshot</strong> of the payme-mock console, captured from a real
  running stand. Links and filters work; anything that would change something is
  inert &mdash;
  <a href="https://github.com/bakhod1r/payme-mock" style="color:#7ab7ff">run it locally</a>
  for the working console.
</div>
<style>
  /* The console never styles a disabled control, because it never has one: on
     the stand every button does something. Here they are the exception that has
     to read as one, or a blue Add button that ignores the click looks broken
     rather than inert. */
  [disabled] { opacity: .45; cursor: not-allowed; filter: grayscale(.6); }
</style>
""".replace("böd", "b6d")


def filename(path: str) -> str:
    """One page address to one flat filename.

    The query is part of the name, not dropped. A filter is a query, so a
    snapshot that threw it away would publish a screen whose every filter link
    led back to the unfiltered page — visibly broken, and the first thing a
    reader tries.
    """
    parsed = urllib.parse.urlparse(path)
    stem = parsed.path.strip("/")
    if stem in ("", "dashboard"):
        stem = "index"

    name = stem.replace("/", "-")
    if parsed.query:
        # Sorted so the same filter reached from two screens is one file.
        pairs = sorted(urllib.parse.parse_qsl(parsed.query))
        slug = "-".join(f"{k}-{v}" for k, v in pairs if v != "")
        if slug:
            name += "--" + re.sub(r"[^A-Za-z0-9._-]+", "_", slug)

    # Long filter combinations would otherwise outrun the filesystem.
    if len(name) > 120:
        name = name[:110] + "-" + hashlib.sha1(name.encode()).hexdigest()[:8]

    return name + ".html"


def fetch(path: str):
    req = urllib.request.Request(BASE + path, headers={"User-Agent": "snapshot"})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            kind = r.headers.get("Content-Type", "")
            if "text/html" not in kind and "text/plain" not in kind:
                return None
            return r.read().decode("utf-8", "replace")
    except Exception as e:  # a 404 or a page needing state we did not create
        print(f"  skip {path}: {e}")
        return None


def normalise(href: str):
    """A page address worth crawling — path and query — or None."""
    if not href or href.startswith(("#", "http://", "https://", "mailto:")):
        return None
    parsed = urllib.parse.urlparse(urllib.parse.urljoin("/", href))
    # /curl and /delete are actions, not pages.
    if parsed.path.endswith(("/curl", "/delete", "/toggle", "/reset")):
        return None
    if parsed.path in ("/login", "/logout", "/healthz"):
        return None
    # Empty values carry no filter, so ?state=&way= is the bare page.
    pairs = sorted((k, v) for k, v in urllib.parse.parse_qsl(parsed.query) if v != "")
    query = urllib.parse.urlencode(pairs)
    return parsed.path + ("?" + query if query else "")


# The note a dialog carries when its form cannot be submitted here.
DIALOG_NOTE = (
    '<p class="snapshot-note" style="margin:0 0 14px;padding:9px 12px;'
    'border-radius:8px;background:#1d2b3a;border:1px solid #2f4b6d;color:#cfe3ff;'
    'font-size:12.5px">Fill it in and look around — this is the real form. It '
    'cannot be submitted in the snapshot, because there is no stand behind it.</p>'
)


def note_dialog(m: "re.Match[str]") -> str:
    """Explain a dialog whose form has nothing live to submit to."""
    head, body, tail = m.group(1), m.group(2), m.group(3)
    if 'method="get"' in body or "snapshot-note" in body:
        return m.group(0)
    return head + DIALOG_NOTE + body + tail


def disable_submit(inner: str) -> str:
    """Stop a form from being sent without freezing everything inside it.

    Only the act is blocked. The fields stay typeable and the client-side
    helpers — the number generators, the expiry shortcuts, the button that
    closes the dialog — keep working, because they need no server and they are
    most of what makes the form worth opening.

    A button with no type attribute is a submit button; that is the HTML
    default, and forgetting it here would leave a live Create button that
    silently does nothing.
    """
    def fix(m):
        tag = m.group(0)
        if re.search(r'\btype="(button|reset)"', tag):
            return tag
        return tag[:-1] + ' disabled title="Inert in the snapshot">'

    return re.sub(r"<button\b[^>]*>", fix, inner)


def inline_curl(body: str) -> str:
    """Carry each entry's curl command in the markup.

    The console's copy button reads a data-curl attribute when there is one and
    falls back to fetching /traffic/{id}/curl when there is not. A static host
    cannot answer that fetch, so the text is fetched once here and written into
    the attribute — which leaves the button doing exactly what it does on the
    stand, rather than failing quietly under the reader's cursor.
    """
    def fill(m):
        tag, entry = m.group(0), m.group(1)
        if "data-curl=" in tag:
            return tag
        text = fetch(f"/traffic/{entry}/curl")
        if text is None:
            return tag
        return tag[:-1] + f' data-curl="{html.escape(text.strip(), quote=True)}">'

    return re.sub(r'<button\b[^>]*\bdata-entry="(\d+)"[^>]*>', fill, body)


def form_defaults(body: str) -> dict:
    """The value each GET filter sends when the reader has chosen nothing.

    The console's sort control always submits something, so without this every
    submission would look like a two-filter combination and match none of the
    single-filter pages the crawl captured.
    """
    out = {}
    for form in re.findall(r'<form[^>]*method="get"[^>]*>(.*?)</form>', body, re.S):
        for name, block in re.findall(r'<select[^>]*name="([^"]+)"(.*?)</select>', form, re.S):
            chosen = re.search(r'<option[^>]*\bselected\b[^>]*value="([^"]*)"', block) or \
                     re.search(r'value="([^"]*)"[^>]*\bselected\b', block)
            out[name] = chosen.group(1) if chosen else ""

        # A hidden field is submitted whatever the reader does — it names the
        # tab, not a choice — so it counts as a default too. Treated as a choice
        # it would make every submission look like a filter combination the
        # crawl never captured.
        for hidden in re.findall(r'<input[^>]*type="hidden"[^>]*>', form):
            name = re.search(r'name="([^"]+)"', hidden)
            value = re.search(r'value="([^"]*)"', hidden)
            if name:
                out[name.group(1)] = value.group(1) if value else ""
    return out


def filter_links(path: str, body: str):
    """Every single-filter address a page's GET forms can reach.

    The console filters with forms rather than links, so these addresses appear
    nowhere in the markup — they have to be built from the options. Only one
    filter at a time is offered: it is what shows the reader what the control
    does, without the crawl walking every pairing.
    """
    base = urllib.parse.urlparse(path).path
    out = []
    for form in re.findall(r'<form[^>]*method="get"[^>]*>(.*?)</form>', body, re.S):
        for name, block in re.findall(r'<select[^>]*name="([^"]+)"(.*?)</select>', form, re.S):
            for value in re.findall(r'value="([^"]*)"', block):
                if value:
                    out.append(base + "?" + urllib.parse.urlencode({name: value}))
    return out


def main() -> None:
    os.makedirs(OUT, exist_ok=True)

    # The traffic log is a tail of dozens of entries and every one has a page.
    # A handful shows what an entry looks like; the rest is weight for no extra
    # information, and links to them fall back to the list.
    caps = {"traffic": 8}
    taken: dict[str, int] = {}

    def capped(path: str) -> bool:
        parts = urllib.parse.urlparse(path).path.strip("/").split("/")
        if len(parts) < 2:
            return False
        limit = caps.get(parts[0])
        if limit is None:
            return False
        taken[parts[0]] = taken.get(parts[0], 0) + 1
        return taken[parts[0]] > limit

    defaults: dict[str, str] = {}
    pages, seen = {}, set(SEEDS)
    queue = [(s, False) for s in SEEDS]
    while queue:
        if len(pages) >= MAX_PAGES:
            print(f"  stopping at {MAX_PAGES} pages")
            break
        path, filtered = queue.pop(0)
        if capped(path):
            continue
        body = fetch(path)
        if body is None:
            continue
        pages[path] = body
        # Only an unfiltered screen shows what the controls send by default; on
        # a filtered one the reader's own choice is what sits selected.
        if "?" not in path:
            defaults.update(form_defaults(body))
        print(f"  got {path}")
        found = re.findall(r'href="([^"]*)"', body)
        if not filtered:
            found += filter_links(path, body)
        for href in found:
            nxt = normalise(href)
            if not nxt or nxt in seen:
                continue
            # One filter deep: a page reached by a filter does not contribute
            # its own filter links, or the crawl walks every combination.
            if filtered and "?" in nxt:
                continue
            seen.add(nxt)
            queue.append((nxt, filtered or "?" in nxt))

    known = {p: filename(p) for p in pages}

    # The console filters with GET forms, which a static host cannot answer.
    # This turns a submission into the file the crawl already wrote, using the
    # same naming rule as filename() above. A combination nobody captured falls
    # back to the unfiltered screen rather than a 404.
    shim = """
<script>
(function () {
  var pages = %s;
  var defaults = %s;
  function navigate(form) {
    var stem = (form.getAttribute('action') || '').replace(/^\\/|\\/$/g, '') || 'index';
    if (stem === 'dashboard') stem = 'index';
    stem = stem.replace(/\\//g, '-');

    var pairs = [];
    new FormData(form).forEach(function (value, key) {
      if (value !== '') pairs.push([key, value]);
    });
    pairs.sort(function (a, b) { return a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0; });

    function nameFor(list) {
      if (!list.length) return stem;
      return stem + '--' + list.map(function (p) { return p[0] + '-' + p[1]; })
        .join('-').replace(/[^A-Za-z0-9._-]+/g, '_');
    }

    // Candidates, most specific first: everything submitted; then the same
    // without the controls sitting at their default, which is what the reader
    // actually chose; then each chosen filter alone, since the snapshot holds
    // one filter at a time; then the bare screen.
    var chosen = pairs.filter(function (p) { return defaults[p[0]] !== p[1]; });
    var tries = [nameFor(pairs), nameFor(chosen)];
    chosen.forEach(function (p) { tries.push(nameFor([p])); });
    tries.push(stem);

    for (var i = 0; i < tries.length; i++) {
      if (pages.indexOf(tries[i] + '.html') >= 0) {
        location.href = tries[i] + '.html';
        return;
      }
    }
    location.href = stem + '.html';
  }

  document.addEventListener('submit', function (e) {
    if (!e.target.matches('form[method="get"]')) return;
    e.preventDefault();
    navigate(e.target);
  });

  // A form submitted from script fires no submit event at all, so the listener
  // above never sees it and the browser leaves for a path this host does not
  // have. The console does exactly that when a filter select changes, so the
  // method itself is routed through the same mapping.
  var nativeSubmit = HTMLFormElement.prototype.submit;
  HTMLFormElement.prototype.submit = function () {
    if (this.getAttribute('method') === 'get') { navigate(this); return; }
    // A POST form has nothing here to submit to; doing nothing is the honest
    // answer, and nativeSubmit is kept so the intent stays readable.
    void nativeSubmit;
  };
})();
</script>
""" % (json.dumps(sorted(known.values())), json.dumps(defaults))

    for path, body in pages.items():
        def rewrite(m):
            raw = m.group(1)
            target = normalise(raw)
            if target is None:
                # An action, an anchor or an external link: leave anchors and
                # external links alone, neutralise the rest.
                if raw.startswith(("#", "http://", "https://")):
                    return m.group(0)
                return 'href="#" data-inert="1"'
            # A detail page the crawl capped falls back to its list, which is
            # where the reader was going to end up anyway.
            if target in known:
                return 'href="%s"' % known[target]
            section = urllib.parse.urlparse(target).path.strip("/").split("/")[0]
            fallback = filename("/" + section)
            if os.path.basename(fallback) in known.values():
                return 'href="%s"' % fallback
            return 'href="#" data-inert="1"'

        out = re.sub(r'href="([^"]*)"', rewrite, body)
        out = inline_curl(out)

        # The live panel reloads the page every five seconds. Nothing in a
        # snapshot ever changes, so left on it would reload forever and throw
        # away the reader's scroll position each time.
        out = out.replace(
            '<input type="checkbox" id="live-refresh" checked',
            '<input type="checkbox" id="live-refresh" disabled'
            ' title="Nothing changes in the snapshot"',
        )

        # A POST form mutates, so it is neutralised: left alone it would submit
        # into the void and land the reader on a 405. A GET form only filters,
        # and the shim below turns its submission into the matching file, so it
        # is left working — the filters are most of what the screens are for.
        def neuter(m):
            tag = m.group(0)
            if 'method="get"' in tag:
                return tag
            return '<form onsubmit="return false" data-inert="1">'

        parts, cursor = [], 0
        for m in re.finditer(r"<form\b[^>]*>(.*?)</form>", out, re.S):
            parts.append(out[cursor:m.start()])
            head = re.match(r"<form\b[^>]*>", m.group(0)).group(0)
            inner = m.group(1)
            if 'method="get"' not in head:
                inner = disable_submit(inner)
                head = '<form onsubmit="return false" data-inert="1">'
            parts.append(head + inner + "</form>")
            cursor = m.end()
        parts.append(out[cursor:])
        out = "".join(parts)

        # A dialog that creates something cannot submit here, but it is worth
        # opening anyway: the form is the clearest description of what the stand
        # lets you rig, and a reader who cannot see it learns nothing. So the
        # opener stays live, the fields stay typeable, and the dialog says at the
        # top why the button at the bottom will not fire.
        out = re.sub(r'(<dialog\b[^>]*>)(.*?)(</dialog>)', note_dialog, out, flags=re.S)

        out = out.replace("</body>", shim + "</body>", 1)

        out = out.replace("<body>", "<body>" + BANNER, 1)
        if BANNER not in out:  # a <body> carrying attributes
            out = re.sub(r"(<body[^>]*>)", r"\1" + BANNER.replace("\\", "\\\\"), out, count=1)

        dest = os.path.join(OUT, known[path])
        with open(dest, "w") as f:
            f.write(out)

    print(f"\n{len(pages)} pages -> {OUT}")
    for p in sorted(known.values()):
        print("   ", p)


if __name__ == "__main__":
    main()
