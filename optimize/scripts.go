package optimize

import (
	"bytes"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultDelayed are the scripts delayed by default (see
// Options.DelayScripts): text found in the src, id or code of analytics,
// tag manager, advertising, chat, social and marketing scripts, which
// aren't needed to show a page, but compete with it for the browser's
// attention while it loads.
var DefaultDelayed = []string{
	// Analytics and tag managers
	"googletagmanager.com", "google-analytics.com", "gtag(", "static.hotjar.com", "clarity.ms",
	"cdn.segment.com", "cdn.mxpnl.com", "stats.wp.com", "plausible.io/js",
	// Advertising and conversion pixels
	"connect.facebook.net", "fbevents.js", "analytics.tiktok.com", "snap.licdn.com",
	"static.ads-twitter.com", "s.pinimg.com", "pagead2.googlesyndication.com", "adsbygoogle",
	"securepubads.g.doubleclick.net", "googletagservices.com", "c.amazon-adsystem.com",
	// Chat and support widgets
	"widget.intercom.io", "client.crisp.chat", "embed.tawk.to", "js.driftt.com",
	"static.zdassets.com", "cdn.livechatinc.com", "js.hs-scripts.com", "cdn.onesignal.com",
	// Marketing and social
	"static.klaviyo.com", "chimpstatic.com", "a.omappapi.com", "s7.addthis.com",
	"platform-api.sharethis.com", "platform.twitter.com/widgets.js",
}

// DelayedType is the type delayed scripts are given, which browsers don't
// run
const DelayedType = "text/honey-delayed"

var (
	attrPattern = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+)))?`)
)

// attributes returns a tag's attributes (the text after its name), by
// lower case name: the first of each
func attributes(tag []byte) map[string]string {
	attrs := map[string]string{}
	for _, a := range attrPattern.FindAllSubmatch(tag, -1) {
		name := strings.ToLower(string(a[1]))
		if _, dup := attrs[name]; !dup {
			attrs[name] = html.UnescapeString(string(a[2]) + string(a[3]) + string(a[4]))
		}
	}
	return attrs
}

// withoutAttributes returns a tag's attribute text without the attributes
// named
func withoutAttributes(tag []byte, names ...string) []byte {
	var b bytes.Buffer
	last := 0
	for _, m := range attrPattern.FindAllSubmatchIndex(tag, -1) {
		name := strings.ToLower(string(tag[m[2]:m[3]]))
		for _, n := range names {
			if name == n {
				// With the space before it
				b.Write(bytes.TrimRight(tag[last:m[0]], " \t\r\n"))
				last = m[1]
				break
			}
		}
	}
	b.Write(tag[last:])
	return b.Bytes()
}

// javaScript returns whether a script's type attribute (if it has one)
// makes it one which browsers run
func javaScript(attrs map[string]string) bool {
	t, ok := attrs["type"]
	if !ok {
		return true
	}
	t = strings.ToLower(strings.TrimSpace(t))
	return t == "" || t == "module" || strings.Contains(t, "javascript") || strings.Contains(t, "ecmascript")
}

// delays returns whether s should be delayed
func (o *Optimizer) delays(p *page, s script) bool {
	if !javaScript(s.attributeMap) {
		return false
	}
	if _, keep := s.attributeMap["data-honey-keep"]; keep {
		return false
	}
	text := strings.ToLower(s.attributeMap["src"] + " " + s.attributeMap["id"] + " " + string(p.body[s.content[0]:s.content[1]]))
	for _, k := range o.keep {
		if strings.Contains(text, k) {
			return false
		}
	}
	for _, d := range o.delay {
		if strings.Contains(text, d) {
			return true
		}
	}
	return false
}

// delayScripts returns the edits which delay p's scripts which should be
func (o *Optimizer) delayScripts(p *page) []edit {
	var edits []edit
	for _, s := range p.scripts {
		if !o.delays(p, s) {
			continue
		}
		attrs := p.body[s.attrs[0]:s.attrs[1]]
		var b bytes.Buffer
		b.WriteString(`<script type="` + DelayedType + `"`)
		if t, ok := s.attributeMap["type"]; ok {
			b.WriteString(` data-honey-type="` + html.EscapeString(t) + `"`)
		}
		if src, ok := s.attributeMap["src"]; ok {
			// Not just the type, so that nothing fetches it early
			b.WriteString(` data-honey-src="` + html.EscapeString(src) + `"`)
		}
		rest := bytes.TrimSpace(withoutAttributes(attrs, "type", "src"))
		if len(rest) > 0 {
			b.WriteByte(' ')
			b.Write(rest)
		}
		b.WriteByte('>')
		b.Write(p.body[s.content[0]:s.end])
		edits = append(edits, edit{start: s.start, end: s.end, text: b.Bytes(), delayed: true})
	}
	return edits
}

// delayLoaderScript runs delayed scripts when the visitor first interacts
// with the page (or after TIMEOUT milliseconds, if it isn't 0), once it has
// loaded: in the page's order, waiting for each external one (unless it's
// async), as the browser would have.  Listeners they add for the page's
// load events, which have been and gone, are called once they've all run,
// and document.write adds to the page rather than replacing it.
const delayLoaderScript = `(function(){var T=TIMEOUT,E=["keydown","mousedown","mousemove","touchstart","touchmove","wheel","scroll"],d=document,w=window,started=false,timer;
function start(){if(started)return;started=true;E.forEach(function(e){w.removeEventListener(e,start,{passive:true})});clearTimeout(timer);if(d.readyState==="loading"){d.addEventListener("DOMContentLoaded",run)}else{run()}}
function run(){var q=[].slice.call(d.querySelectorAll('script[type="` + DelayedType + `"]')),dAdd=d.addEventListener,wAdd=w.addEventListener,write=d.write,writeln=d.writeln,later=[],onload=w.onload,cur=null;
d.addEventListener=function(t,f,o){if(t==="DOMContentLoaded"||t==="readystatechange"){later.push([d,t,f])}else{dAdd.call(d,t,f,o)}};
w.addEventListener=function(t,f,o){if(t==="load"||t==="DOMContentLoaded"){later.push([w,t,f])}else{wAdd.call(w,t,f,o)}};
d.write=d.writeln=function(h){if(cur&&cur.parentNode){cur.insertAdjacentHTML("afterend",h)}else{d.body.insertAdjacentHTML("beforeend",h)}};
function done(){d.addEventListener=dAdd;w.addEventListener=wAdd;d.write=write;d.writeln=writeln;later.forEach(function(l){try{var e=new Event(l[1]);typeof l[2]==="function"?l[2].call(l[0],e):l[2].handleEvent(e)}catch(x){setTimeout(function(){throw x})}});if(w.onload&&w.onload!==onload){try{w.onload(new Event("load"))}catch(x){setTimeout(function(){throw x})}}}
function next(){var s=q.shift();if(!s){done();return}var n=d.createElement("script");[].forEach.call(s.attributes,function(a){if(a.name!=="type"&&a.name!=="data-honey-type"&&a.name!=="data-honey-src")n.setAttribute(a.name,a.value)});
var t=s.getAttribute("data-honey-type");if(t)n.setAttribute("type",t);var src=s.getAttribute("data-honey-src");cur=n;
if(src!==null){var waits=!s.hasAttribute("async");if(waits){n.onload=n.onerror=next}n.src=src;s.parentNode.replaceChild(n,s);if(!waits)next()}else{n.text=s.text;s.parentNode.replaceChild(n,s);next()}}
next()}
E.forEach(function(e){w.addEventListener(e,start,{passive:true})});if(T)timer=setTimeout(start,T)})();`

// delayLoader returns the script which runs delayed scripts (see
// delayLoaderScript)
func delayLoader(timeout time.Duration) []byte {
	return []byte(`<script id="honey-delay">` + strings.Replace(delayLoaderScript, "TIMEOUT", strconv.FormatInt(timeout.Milliseconds(), 10), 1) + `</script>`)
}
