// Package helplayout reflows unstyled key-and-description items into rows for
// aligned terminal help and footer displays.
//
// The package makes layout decisions only. It does not style or render text
// and has no dependency on Bubble Tea, bubbles, or lipgloss. Callers remain
// responsible for rendering the returned rows with their own Charm version or
// another terminal UI stack.
//
// ReflowRows measures plain item text in terminal cells. Styling should be
// applied after reflow and should not change the visible item text. Callers
// pass the complete visible width added between columns, including any border,
// separator, or padding cells used by their renderer.
package helplayout
