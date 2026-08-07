package screen

import (
	"fmt"

	uv "github.com/charmbracelet/ultraviolet"
)

// Grid is a test-only, comparable snapshot of a Screen's full state: every
// cell (content, style, hyperlink), cursor position/visibility, the
// alt-screen flag, the sniffed mode set, and the title. It is the
// substrate for the grid-equivalence keystone test (design doc §10.2): two
// Grids taken from two independently-fed emulators are compared field by
// field and cell by cell.
type Grid struct {
	Rows, Cols int
	Cells      [][]uv.Cell // [row][col], row-major, len(Cells) == Rows, len(Cells[i]) == Cols

	CursorX, CursorY int
	CursorVisible    bool

	AltScreen bool
	Modes     map[int]bool
	Title     string
}

// Equal reports whether g and o represent the same visual state: same
// dimensions, same cursor position/visibility, same alt-screen flag, same
// title, same mode set, and every cell pairwise Equal (uv.Cell.Equal
// compares content, width, style, and hyperlink).
func (g Grid) Equal(o Grid) (bool, string) {
	if g.Rows != o.Rows || g.Cols != o.Cols {
		return false, fmt.Sprintf("dimensions differ: %dx%d vs %dx%d", g.Rows, g.Cols, o.Rows, o.Cols)
	}
	if g.CursorX != o.CursorX || g.CursorY != o.CursorY {
		return false, fmt.Sprintf("cursor position differs: (%d,%d) vs (%d,%d)", g.CursorX, g.CursorY, o.CursorX, o.CursorY)
	}
	if g.CursorVisible != o.CursorVisible {
		return false, fmt.Sprintf("cursor visibility differs: %v vs %v", g.CursorVisible, o.CursorVisible)
	}
	if g.AltScreen != o.AltScreen {
		return false, fmt.Sprintf("alt-screen flag differs: %v vs %v", g.AltScreen, o.AltScreen)
	}
	if g.Title != o.Title {
		return false, fmt.Sprintf("title differs: %q vs %q", g.Title, o.Title)
	}
	if len(g.Modes) != len(o.Modes) {
		return false, fmt.Sprintf("mode set size differs: %v vs %v", g.Modes, o.Modes)
	}
	for m, v := range g.Modes {
		if ov, ok := o.Modes[m]; !ok || ov != v {
			return false, fmt.Sprintf("mode %d differs: %v vs %v (present=%v)", m, v, ov, ok)
		}
	}
	for y := 0; y < g.Rows; y++ {
		for x := 0; x < g.Cols; x++ {
			a, b := g.Cells[y][x], o.Cells[y][x]
			if !a.Equal(&b) {
				return false, fmt.Sprintf(
					"cell (row=%d,col=%d) differs:\n  a: content=%q width=%d style=%+v link=%+v\n  b: content=%q width=%d style=%+v link=%+v",
					y, x, a.Content, a.Width, a.Style, a.Link, b.Content, b.Width, b.Style, b.Link,
				)
			}
		}
	}
	return true, ""
}

// DebugGrid returns the Screen's current state as a Grid. Test-only: it
// exists specifically to give the grid-equivalence keystone test (§10.2)
// something to compare, cell by cell, between two independently-fed
// emulators.
func (s *Screen) DebugGrid() Grid {
	s.mu.Lock()
	defer s.mu.Unlock()

	w, h := s.emu.Width(), s.emu.Height()
	g := Grid{
		Rows:          h,
		Cols:          w,
		AltScreen:     s.emu.IsAltScreen(),
		CursorVisible: s.cursorVisible,
		Title:         s.title,
		Modes:         make(map[int]bool, len(s.modes)),
	}

	pos := s.emu.CursorPosition()
	g.CursorX, g.CursorY = pos.X, pos.Y

	for k, v := range s.modes {
		g.Modes[k] = v
	}

	g.Cells = make([][]uv.Cell, h)
	for y := 0; y < h; y++ {
		row := make([]uv.Cell, w)
		for x := 0; x < w; x++ {
			if c := s.emu.CellAt(x, y); c != nil {
				row[x] = *c
			}
		}
		g.Cells[y] = row
	}
	return g
}
