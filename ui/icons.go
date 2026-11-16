package workspace

import "github.com/egoist/mygo/ui"

// Original, small vector icons: no icon font, network request, or glyph dependency.
func svg(path string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">` + path + `</svg>`))
}

var (
	iFolder   = svg(`<path d="M3 7a2 2 0 0 1 2-2h5l2 2h7a2 2 0 0 1 2 2v10H3Z"/>`)
	iFile     = svg(`<path d="M6 3h8l4 4v14H6Z"/><path d="M14 3v5h4M9 13h6M9 17h4"/>`)
	iBucket   = svg(`<rect x="3" y="5" width="18" height="16" rx="3"/><path d="M7 2h10M3 10h18M10 14h4"/>`)
	iUpload   = svg(`<path d="M12 16V3m-5 5 5-5 5 5M4 15v5h16v-5"/>`)
	iPlus     = svg(`<path d="M12 5v14M5 12h14"/>`)
	iAccount  = svg(`<circle cx="12" cy="8" r="4"/><path d="M4 21v-2a8 8 0 0 1 16 0v2"/>`)
	iSettings = svg(`<path d="M4 7h16M4 17h16"/><circle cx="9" cy="7" r="3"/><circle cx="15" cy="17" r="3"/>`)
	iRefresh  = svg(`<path d="M20 8a9 9 0 1 0 1 7M20 3v5h-5"/>`)
	iChevron  = svg(`<path d="m9 5 7 7-7 7"/>`)
	iSearch   = svg(`<circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 5 5"/>`)
	iDelete   = svg(`<path d="M4 6h16M9 6V3h6v3M6 6l1 15h10l1-15M10 10v7m4-7v7"/>`)
	iEdit     = svg(`<path d="m4 16 12-12 4 4L8 20H4ZM13 7l4 4"/>`)
	iMove     = svg(`<path d="M3 6h8M3 12h14m-4-4 4 4-4 4M3 18h8M21 4v16"/>`)
	iCopy     = svg(`<rect x="8" y="8" width="13" height="13" rx="2"/><path d="M16 4V3H3v13h1"/>`)
	iClose    = svg(`<path d="m6 6 12 12M6 18 18 6"/>`)
	iMinimize = svg(`<path d="M5 12h14"/>`)
	iMaximize = svg(`<rect x="5" y="5" width="14" height="14" rx="2"/>`)
	iRestore  = svg(`<rect x="4" y="9" width="11" height="11" rx="2"/><path d="M8 9V6a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-3"/>`)
	iCheck    = svg(`<path d="m5 12 4 4L19 6"/>`)
	iCloud    = svg(`<path d="M7 19a5 5 0 0 1-1-10 7 7 0 0 1 13-1 5.5 5.5 0 0 1-1 11Z"/><path d="M9 14h6m-3-3v6"/>`)
	iLock     = svg(`<rect x="5" y="10" width="14" height="11" rx="3"/><path d="M8 10V6a4 4 0 0 1 8 0v4m-4 5v2"/>`)
	iInfo     = svg(`<circle cx="12" cy="12" r="9"/><path d="M12 11v6m0-10v.1"/>`)
)
