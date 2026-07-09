// Package screen composes already-rendered terminal lines and screens.
// It measures terminal cells rather than bytes or runes and treats ANSI
// control sequences as zero-width data that must not be split.
//
// Overlay coordinates are zero-based: row 0, column 0 is the top-left cell of
// the declared viewport. OverlayAt clips the panel against that viewport; it
// does not move an out-of-bounds panel back on screen. Negative coordinates
// therefore display only the intersecting bottom or right portion. A panel is
// rectangular, with the width of its widest line, and shorter lines erase the
// remainder of their row in that rectangle.
//
// Width is measured in grapheme clusters using github.com/charmbracelet/x/ansi.
// A wide grapheme is atomic: if a cut crosses it, the complete grapheme is
// omitted and Splice pads any uncovered cells with spaces. This package uses
// the dependency's ANSI state machine instead of a smaller CSI/OSC-only parser
// so that CSI, OSC, DCS, APC, C1 controls, and modern Unicode width rules share
// one implementation. Inputs must contain valid UTF-8 and complete ANSI
// sequences. The helpers preserve, but do not validate or sanitize, control
// sequences; an incomplete or malformed sequence can cause following bytes to
// be interpreted as part of that sequence and measured as zero-width.
// Cursor-moving, cursor-saving, and erasing controls are likewise preserved as
// zero-width bytes, but their terminal side effects are not modeled; screen
// geometry is intended for line-local styling and hyperlink sequences.
package screen
