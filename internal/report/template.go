package report

import (
	"fmt"
	"html/template"
	"time"
)

var funcs = template.FuncMap{
	"bytes":   humanBytes,
	"num":     humanCount,
	"pct":     func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
	"stamp":   func(t time.Time) string { return t.Format("2 January 2006, 15:04 MST") },
	"title":   titleCase,
	"nonzero": func(n int64) bool { return n > 0 },
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func humanCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 32
	}
	return string(b)
}

var pageTemplate = template.Must(template.New("report").Funcs(funcs).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Photo Sync Report</title>
<style>
  :root {
    --bg: #fbfaf8;  --panel: #ffffff;  --ink: #1c1b19;  --muted: #6b6862;
    --line: #e4e1db; --accent: #2f6f4e; --warn: #a8621b; --bad: #a3352b;
    --bar-bg: #ece9e3;
  }
  :root:not([data-theme="light"]) { }
  @media (prefers-color-scheme: dark) {
    :root:not([data-theme="light"]) {
      --bg: #16181a; --panel: #1e2124; --ink: #e8e6e3; --muted: #9b9892;
      --line: #2f3337; --accent: #7fc3a0; --warn: #e2a35f; --bad: #e58a7f;
      --bar-bg: #2a2e32;
    }
  }
  :root[data-theme="dark"] {
    --bg: #16181a; --panel: #1e2124; --ink: #e8e6e3; --muted: #9b9892;
    --line: #2f3337; --accent: #7fc3a0; --warn: #e2a35f; --bad: #e58a7f;
    --bar-bg: #2a2e32;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--ink);
    font: 15px/1.55 ui-sans-serif, -apple-system, "Segoe UI", system-ui, sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  .wrap { max-width: 1080px; margin: 0 auto; padding: 48px 24px 96px; }
  header { border-bottom: 1px solid var(--line); padding-bottom: 24px; margin-bottom: 36px; }
  h1 { font-size: 28px; margin: 0 0 6px; letter-spacing: -0.01em; }
  h2 { font-size: 18px; margin: 44px 0 14px; letter-spacing: -0.005em; }
  h3 { font-size: 14px; margin: 26px 0 10px; color: var(--muted); font-weight: 600;
       text-transform: uppercase; letter-spacing: 0.06em; }
  p  { margin: 0 0 12px; }
  .sub { color: var(--muted); font-size: 14px; }
  code, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12.5px; }

  .tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); gap: 14px; margin: 24px 0; }
  .tile { background: var(--panel); border: 1px solid var(--line); border-radius: 10px; padding: 16px 18px; }
  .tile .v { font-size: 25px; font-weight: 620; letter-spacing: -0.02em; }
  .tile .k { color: var(--muted); font-size: 12.5px; margin-top: 3px; }
  .tile .x { color: var(--muted); font-size: 12px; margin-top: 8px; }

  .scroll { overflow-x: auto; -webkit-overflow-scrolling: touch; }
  table { border-collapse: collapse; width: 100%; font-size: 13.5px; min-width: 520px; }
  th, td { text-align: left; padding: 8px 12px 8px 0; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { color: var(--muted); font-weight: 600; font-size: 12px; text-transform: uppercase; letter-spacing: 0.05em; }
  td.n, th.n { text-align: right; font-variant-numeric: tabular-nums; padding-right: 18px; }
  tbody tr:last-child td { border-bottom: none; }

  .bar { background: var(--bar-bg); border-radius: 3px; height: 6px; width: 90px; overflow: hidden; display: inline-block; vertical-align: middle; }
  .bar > i { display: block; height: 100%; background: var(--accent); }

  .note { background: var(--panel); border: 1px solid var(--line); border-left: 3px solid var(--warn);
          border-radius: 8px; padding: 14px 18px; margin: 18px 0; font-size: 13.5px; }
  .note ul { margin: 8px 0 0; padding-left: 18px; }
  .note li { margin-bottom: 6px; }
  .bad { color: var(--bad); }
  .warn { color: var(--warn); }
  .ok { color: var(--accent); }
  footer { margin-top: 64px; padding-top: 20px; border-top: 1px solid var(--line); color: var(--muted); font-size: 12.5px; }
</style>
</head>
<body>
<div class="wrap">

<header>
  <h1>Photo Sync Report</h1>
  <p class="sub">OneDrive <code>{{.SourceRoot}}</code> → Google Photos library<br>
     Generated {{stamp .Generated}}</p>
</header>

<div class="tiles">
  <div class="tile">
    <div class="v">{{num .SourceFiles}}</div>
    <div class="k">files in OneDrive</div>
    <div class="x">{{bytes .SourceBytes}} of originals</div>
  </div>
  <div class="tile">
    <div class="v">{{num .DriveFiles}}</div>
    <div class="k">files in the Drive archive</div>
    <div class="x">{{bytes .DriveBytes}} under <code>{{.TargetTree}}</code></div>
  </div>
  {{range .States}}{{if eq .Label "created"}}
  <div class="tile">
    <div class="v ok">{{num .Files}}</div>
    <div class="k">uploaded to the library</div>
    <div class="x">{{bytes .Bytes}} transferred</div>
  </div>
  {{end}}{{end}}
  {{range .States}}{{if eq .Label "failed"}}{{if nonzero .Files}}
  <div class="tile">
    <div class="v bad">{{num .Files}}</div>
    <div class="k">failed</div>
    <div class="x">{{bytes .Bytes}} not transferred</div>
  </div>
  {{end}}{{end}}{{end}}
</div>

<h2>What the comparison found</h2>
<p>Every OneDrive file was matched against the Google Photos tree in Drive on
   filename and exact byte size. The size half is what separates an original
   from the re-encode sitting under the same name.</p>
<div class="scroll">
<table>
  <thead><tr><th>Verdict</th><th class="n">Files</th><th class="n">Bytes</th><th>Meaning</th></tr></thead>
  <tbody>
  {{range .Verdicts}}
    <tr><td><strong>{{title .Label}}</strong></td><td class="n">{{num .Files}}</td>
        <td class="n">{{bytes .Bytes}}</td><td class="sub">{{.Note}}</td></tr>
  {{end}}
  </tbody>
</table>
</div>

{{if .Samples}}
<h3>Largest gaps between original and Drive copy</h3>
<div class="scroll">
<table>
  <thead><tr><th>File</th><th class="n">OneDrive original</th><th class="n">Drive copy</th><th class="n">Ratio</th></tr></thead>
  <tbody>
  {{range .Samples}}
    <tr><td class="mono">{{.Name}}</td><td class="n">{{bytes .Size}}</td>
        <td class="n">{{bytes .DriveSize}}</td><td class="n">{{.Ratio}}</td></tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}

<h2>Transfer state</h2>
<div class="scroll">
<table>
  <thead><tr><th>State</th><th class="n">Files</th><th class="n">Bytes</th><th>Meaning</th></tr></thead>
  <tbody>
  {{range .States}}
    <tr><td><strong>{{title .Label}}</strong></td><td class="n">{{num .Files}}</td>
        <td class="n">{{bytes .Bytes}}</td><td class="sub">{{.Note}}</td></tr>
  {{else}}
    <tr><td colspan="4" class="sub">No transfer has been started yet.</td></tr>
  {{end}}
  </tbody>
</table>
</div>

<h2>By file type</h2>
<div class="scroll">
<table>
  <thead><tr><th>Type</th><th>Kind</th><th class="n">Files</th><th class="n">Bytes</th>
             <th class="n">Uploaded</th><th class="n">Pending</th><th class="n">Failed</th><th>Progress</th></tr></thead>
  <tbody>
  {{range .Types}}
    <tr><td class="mono">.{{.Ext}}</td><td class="sub">{{.Kind}}</td>
        <td class="n">{{num .Files}}</td><td class="n">{{bytes .Bytes}}</td>
        <td class="n">{{num .Created}}</td><td class="n">{{num .Pending}}</td>
        <td class="n">{{if nonzero .Failed}}<span class="bad">{{num .Failed}}</span>{{else}}0{{end}}</td>
        <td><span class="bar"><i style="width:{{pct .Progress}}"></i></span></td></tr>
  {{end}}
  </tbody>
</table>
</div>

<h2>By album</h2>
<p class="sub">Each OneDrive folder becomes one album in the Google Photos library, so the
   archive's structure survives the move.</p>
<div class="scroll">
<table>
  <thead><tr><th>Album</th><th class="n">Files</th><th class="n">Bytes</th>
             <th class="n">Uploaded</th><th class="n">Failed</th><th>Progress</th></tr></thead>
  <tbody>
  {{range .Albums}}
    <tr><td class="mono">{{.Album}}</td><td class="n">{{num .Files}}</td>
        <td class="n">{{bytes .Bytes}}</td><td class="n">{{num .Created}}</td>
        <td class="n">{{if nonzero .Failed}}<span class="bad">{{num .Failed}}</span>{{else}}0{{end}}</td>
        <td><span class="bar"><i style="width:{{pct .Progress}}"></i></span></td></tr>
  {{else}}
    <tr><td colspan="6" class="sub">No work-list yet - run <code>photosync analyse</code>.</td></tr>
  {{end}}
  </tbody>
</table>
</div>

{{if .Failures}}
<h2>Failures</h2>
<div class="scroll">
<table>
  <thead><tr><th>File</th><th class="n">Bytes</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .Failures}}
    <tr><td class="mono">{{.Path}}</td><td class="n">{{bytes .Size}}</td><td class="sub">{{.Reason}}</td></tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}

{{if .Skipped}}
<h2>Deliberately skipped</h2>
<div class="scroll">
<table>
  <thead><tr><th>File</th><th class="n">Bytes</th><th>Reason</th></tr></thead>
  <tbody>
  {{range .Skipped}}
    <tr><td class="mono">{{.Path}}</td><td class="n">{{bytes .Size}}</td><td class="sub">{{.Reason}}</td></tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}

{{if or (nonzero .Prune.DeleteFiles) (nonzero .Prune.ReviewFiles)}}
<h2>Compressed copies in Drive</h2>
<p>Once an original is confirmed in the library, its low-resolution Drive copy can go.
   A copy is only queued for deletion when the match is unambiguous: exactly one
   OneDrive file and one Drive file carry the name, the Drive copy is strictly
   smaller, and the original has a confirmed media item. Everything else is left
   for review rather than deleted.</p>
<div class="tiles">
  <div class="tile"><div class="v">{{num .Prune.DeleteFiles}}</div>
    <div class="k">safe to delete</div><div class="x">{{bytes .Prune.DeleteBytes}}</div></div>
  <div class="tile"><div class="v warn">{{num .Prune.ReviewFiles}}</div>
    <div class="k">held for review</div><div class="x">{{bytes .Prune.ReviewBytes}}</div></div>
  <div class="tile"><div class="v">{{num .Prune.DeletedFiles}}</div>
    <div class="k">actually deleted</div><div class="x">{{bytes .Prune.DeletedBytes}} - recoverable from Drive trash for 30 days</div></div>
</div>
{{if .Prune.Reviews}}
<h3>Held for review</h3>
<div class="scroll">
<table>
  <thead><tr><th>Drive file</th><th class="n">Bytes</th><th>Why it was not deleted</th></tr></thead>
  <tbody>
  {{range .Prune.Reviews}}
    <tr><td class="mono">{{.Path}}</td><td class="n">{{bytes .Size}}</td><td class="sub">{{.Reason}}</td></tr>
  {{end}}
  </tbody>
</table>
</div>
{{end}}
{{end}}

<h2>What these numbers cover</h2>
<div class="note">
<ul>
  <li><strong>The library itself cannot be enumerated.</strong> Since 31 March 2025 an
      application may read back only the media it created, so "already present" is
      judged against the Google Photos folder in Drive, not against the live library.
      Google Photos deduplicates byte-identical uploads within an account, so a file
      that was already there is not duplicated by being sent again.</li>
  <li><strong>Originals may land beside existing lower-quality copies.</strong> If a photo
      reached the library previously in "storage saver" quality, its bytes differ from
      the OneDrive original, so deduplication will not merge them and both will exist.
      That is the intended outcome of asking for originals, not a fault.</li>
  <li><strong>Identity is name plus exact byte size,</strong> not a content hash. Neither
      cloud exposes a hash the other can be compared against, and computing one would
      mean downloading the entire archive twice over.</li>
  <li><strong>Every row here is backed by a file.</strong> The CSV manifest at
      <code>{{.ManifestPath}}</code> carries one line per file: name, path, size, verdict,
      final state, album, and the media item id the library assigned.</li>
</ul>
</div>

<footer>
  Generated by photosync from its own transfer journal. Counts come from the recorded
  state of each file, not from a re-scan, so the report and the transfer cannot disagree.
</footer>

</div>
</body>
</html>
`))
