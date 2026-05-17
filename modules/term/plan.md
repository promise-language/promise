# Promise `term` Module — API Proposal

## Motivation

Promise has `print`/`print_line` for line-oriented output and `io` for file I/O, but nothing
for building interactive terminal applications. To write a TUI app you need: raw mode
(unbuffered, unechoed input), cursor positioning, styled text, screen clearing, alternate
screen buffers, and structured input events (keys, mouse, resize). Every major systems
language provides these — curses in C/Python, crossterm/termion in Rust, tcell/bubbletea in
Go. Promise needs a `term` module to match.

### Design Principles

1. **Idiomatic Promise** — use `use` binding for cleanup, enums for events, value types for
   styles, bare-call auto-propagation in `!` functions, channels/`go` for async patterns.
2. **Cell-buffer model** — write to an in-memory grid, then `show()` to the terminal.
   This avoids tearing and is the proven model from tcell, termbox, and curses.
3. **Layered** — low-level primitives (set cell, poll event) that higher-level widget
   libraries can build on later.
4. **Cross-platform** — abstracts over POSIX termios + ANSI escapes on Unix and Console API
   on Windows, just as crossterm does.

---

## Quick Start

```
use term;

main()! {
  // use-binding: restores terminal state when screen goes out of scope
  use screen := term.Screen.open();

  screen.clear();
  screen.write(Point(0, 0), "Hello, TUI!", style: term.Style.default());
  screen.show();

  // Block for one event
  term.Event event = screen.poll_event();
  match event {
    term.Event.Key(k) => {
      screen.write(Point(0, 1), "You pressed: {k.name}", style: term.Style.default());
    },
    _ => {},
  }
  screen.show();
}
// <- screen.close() called automatically, terminal restored
```

---

## Assumed `std` Geometry Types

These types live in `std` (auto-imported, no `use` needed) and are shared across
modules. The term module uses `Point[int]`, `Size[int]`, and `Rect[int]`
throughout its API.

```
type Point[T] `public {
  T x `value;
  T y `value;
}

type Size[T] `public {
  T width `value;
  T height `value;
}

type Rect[T] `public {
  T x `value;
  T y `value;
  T width `value;
  T height `value;
}
```

---

## Module Overview

```
use term;
```

The `term` module exports the following public types and functions.
It also relies on `Point[int]`, `Size[int]`, and `Rect[int]` from `std`.

| Type / Function          | Purpose                                               |
| :----------------------- | :---------------------------------------------------- |
| `Screen`                 | Main terminal handle — owns raw mode + alt buffer     |
| `Cell`                   | One character cell: `char` + `Style`                  |
| `Style`                  | Foreground, background, attributes (value type)       |
| `Color`                  | Named colors, 256-palette, and 24-bit RGB             |
| `Attribute`              | Bold, dim, italic, underline, blink, reverse, etc.    |
| `Event`                  | Input event enum: key, mouse, resize, paste, focus    |
| `KeyEvent`               | Key code + modifiers                                  |
| `MouseEvent`             | Button, position, modifiers                           |
| `Key`                    | Enum of known key codes                               |
| `Modifier`               | Bitflags for Shift, Ctrl, Alt, Super                  |
| `MouseButton`            | Enum of mouse buttons                                 |
| `MouseAction`            | Enum: Press, Release, Move, ScrollUp, ScrollDown      |
| `CursorShape`            | Enum: Block, Underline, Bar (+ blinking variants)     |
| `is_terminal()`          | Check if a file descriptor is connected to a TTY      |

---

## `Screen`

The central type. Opening a `Screen` enters raw mode and switches to the alternate screen
buffer. Closing it (explicitly or via `use`) restores the original terminal state.

**TTY fallback:** By default, `Screen.open()` tries stdin/stdout first. If they are not
terminals (e.g. piped), it automatically falls back to the controlling terminal
(`/dev/tty` on Unix, `CONIN$`/`CONOUT$` on Windows) — the same behavior as `less` and
`git`. This means a program piping data through stdin can still draw a TUI. The fallback
can be disabled via `ScreenOptions` if you want `open()` to fail when stdio isn't a TTY.

This is the same lifecycle model as `tcell.Screen.Init()`/`Fini()` in Go and
`crossterm::terminal::enable_raw_mode()` + `EnterAlternateScreen` in Rust.

```
type Screen `public {

  // --- Construction / teardown ---

  // Opens the terminal: enables raw mode, enters alternate screen buffer.
  // Use `use screen := Screen.open();` for automatic cleanup.
  // Pass options to customize behavior; defaults to alternate screen + raw mode.
  // Errors auto-propagate in a ! function; use open()! only to panic.
  open(ScreenOptions options = ScreenOptions()) Self! `factory;

  // Restores terminal: exits alternate screen, disables raw mode.
  // Called automatically at scope exit when bound with `use`.
  close(~this)!;

  // --- Querying ---

  // Terminal dimensions in character cells.
  get size Size[int];

  // --- Cell buffer ---

  // Write a styled string starting at pos. Clips at screen edge.
  // Coordinates are 0-indexed. Handles wide characters correctly.
  write(~this, Point[int] pos, string text, Style style);

  // Write a single character at pos with style.
  set_cell(~this, Point[int] pos, char ch, Style style);

  // Read back a cell from the buffer.
  get_cell(&this, Point[int] pos) Cell;

  // Fill a rectangular region with a character + style.
  fill(~this, Rect[int] rect, char ch, Style style);

  // Clear the entire screen buffer (fills with ' ' and default style).
  clear(~this);

  // Clear a rectangular region.
  clear_region(~this, Rect[int] rect);

  // Set the default style used by clear().
  set_default_style(~this, Style style);

  // --- Rendering ---

  // Push the cell buffer to the terminal.
  // By default, only changed cells are sent (diff-based).
  // Pass full: true to force a complete redraw — use after resize
  // or when an external process may have corrupted the screen.
  show(~this, bool full = false)!;

  // --- Cursor ---

  // Position the hardware cursor. Cursor is visible after show().
  set_cursor(~this, Point[int] pos);

  // Hide the hardware cursor.
  hide_cursor(~this);

  // Change cursor shape (if terminal supports it).
  set_cursor_shape(~this, CursorShape shape);

  // --- Events ---

  // Block until an event is available, then return it.
  poll_event(&this) Event!;

  // Return an event if one is ready; none otherwise. Non-blocking.
  try_poll_event(&this) Event?!;

  // Poll with a timeout. Returns none if the deadline expires.
  poll_event_timeout(&this, int millis) Event?!;

  // Return a channel that receives events. Spawns a reader goroutine.
  // Close the screen or the channel to stop.
  events(&this) channel[Event]!;

  // --- Mouse ---

  enable_mouse(~this)!;
  disable_mouse(~this)!;
  get mouse_enabled bool;

  // --- Paste ---

  // Enable bracketed paste mode. Pasted text arrives as Event.Paste().
  enable_paste(~this)!;
  disable_paste(~this)!;

  // --- Focus ---

  // Enable focus tracking. Focus/blur arrives as Event.Focus().
  enable_focus(~this)!;
  disable_focus(~this)!;

  // --- Subprocess handoff ---

  // Temporarily restore the terminal to its original state (cooked mode,
  // main screen buffer) so a subprocess can take over (e.g. spawning
  // $EDITOR). The event loop is paused. Call resume() when the subprocess
  // exits to re-enter raw mode and do a full redraw.
  // This is what tcell's Suspend()/Resume() provide.
  suspend(~this)!;

  // Re-enter raw mode and alternate screen after a suspend().
  // Automatically does a full redraw (equivalent to show(full: true)).
  resume(~this)!;
}
```

### `ScreenOptions`

```
type ScreenOptions `public {
  bool alternate_screen `value = true;   // use alternate screen buffer
  bool raw_mode `value = true;           // enable raw mode
  bool mouse `value = false;             // enable mouse on open
  bool paste `value = false;             // enable bracketed paste on open
  bool focus `value = false;             // enable focus tracking on open
  bool tty_fallback `value = true;       // fall back to /dev/tty when stdio is not a terminal
}
```

### `is_terminal()`

Utility for CLI tools that don't need a full `Screen` but want to know whether
to enable colors, progress bars, or interactive prompts. Equivalent to Rust's
`std::io::IsTerminal` and Go's `term.IsTerminal(fd)`.

```
// Check if a file descriptor is a terminal.
// Defaults to stdout (fd 1), the most common use.
// 0 = stdin, 1 = stdout, 2 = stderr.
term.is_terminal(int fd = 1) bool;
```

Usage:

```
use term;

main() {
  if term.is_terminal() {
    print_line("\x1b[32mGreen text!\x1b[0m");   // safe to use ANSI colors (checks stdout)
  } else {
    print_line("Plain text");                    // piped, no escapes
  }
}
```

---

## `Cell`

A single cell in the buffer. Value type — cheap to copy and compare.

```
type Cell `public {
  char ch `value;
  Style style `value;
}
```

---

## `Style`

Composable value type for text styling. Inspired by tcell's `Style` builder pattern
and crossterm's `ContentStyle`. All fields are `\`value` so it's stack-allocated and
auto-copied.

```
type Style `public {
  Color? foreground `value;    // none = terminal default
  Color? background `value;    // none = terminal default
  Attribute attr `value;       // default = Attribute.none()

  // Factory: terminal defaults
  default() Self `factory;

  // Builder methods (return new Style, original unchanged)
  foreground(&this, Color color) Style;
  background(&this, Color color) Style;
  bold(&this) Style;
  dim(&this) Style;
  italic(&this) Style;
  underline(&this) Style;
  blink(&this) Style;
  reverse(&this) Style;
  strikethrough(&this) Style;
  reset_attributes(&this) Style;
}
```

### Usage

```
// Styles are value types — chain freely, pass around cheaply
title_style := term.Style.default().foreground(term.Color.Yellow).bold();
error_style := term.Style.default().foreground(term.Color.Red).background(term.Color.Black).bold();
subtle      := term.Style.default().foreground(term.Color.rgb(128, 128, 128)).dim();

screen.write(Point(0, 0), "WARNING", style: error_style);
```

---

## `Color`

Enum covering named ANSI colors, 256-palette, and 24-bit true color.
Models the same color space as crossterm's `Color` and tcell's `Color`.

```
enum Color `public {
  // Default terminal color
  Default,

  // Standard 8 ANSI colors (0-7)
  Black,
  Red,
  Green,
  Yellow,
  Blue,
  Magenta,
  Cyan,
  White,

  // Bright / high-intensity variants (8-15)
  BrightBlack,   // aka "grey" / "dark grey"
  BrightRed,
  BrightGreen,
  BrightYellow,
  BrightBlue,
  BrightMagenta,
  BrightCyan,
  BrightWhite,

  // 256-color palette index (0-255)
  Palette(u8 index),

  // 24-bit true color
  Rgb(u8 r, u8 g, u8 b),

  // Convenience factory for Rgb
  rgb(u8 r, u8 g, u8 b) Color `factory {
    return Color.Rgb(r: r, g: g, b: b);
  }

  // Construct from hex: Color.hex(0xFF8800)
  hex(int value) Color `factory {
    u8 r = (value >> 16) & 0xFF;
    u8 g = (value >> 8) & 0xFF;
    u8 b = value & 0xFF;
    return Color.Rgb(r: r, g: g, b: b);
  }
}
```

---

## `Attribute`

Text decoration attributes. Modeled as a value type with bool fields — consistent with
how `Modifier` is designed in this proposal. Multiple attributes compose naturally via
the builder methods on `Style`.

```
type Attribute `public {
  bool bold `value;
  bool dim `value;
  bool italic `value;
  bool underline `value;
  bool blink `value;
  bool reverse `value;
  bool hidden `value;
  bool strikethrough `value;

  none() Self `factory {
    return Self(
      bold: false, dim: false, italic: false, underline: false,
      blink: false, reverse: false, hidden: false, strikethrough: false,
    );
  }
}
```

---

## `Event`

All terminal input flows through a single enum, exhaustively matchable. This follows the
crossterm `Event` / tcell `EventKey`/`EventMouse`/`EventResize` pattern, but unified into
one enum to play to Promise's `match` strength.

```
enum Event `public {
  Key(KeyEvent key),
  Mouse(MouseEvent mouse),
  Resize(Size[int] size),
  Paste(string text),
  FocusGained,
  FocusLost,
}
```

### `KeyEvent`

```
type KeyEvent `public {
  Key code `value;           // the key
  char? ch `value;           // printable character (for Key.Char)
  Modifier modifiers `value; // Ctrl, Alt, Shift, Super

  // Convenience getters
  get is_ctrl bool => this.modifiers.has_ctrl;
  get is_alt  bool => this.modifiers.has_alt;
  get is_shift bool => this.modifiers.has_shift;
  get name string;  // human-readable: "Ctrl+A", "Enter", "F1", etc.
}
```

### `Key`

Enum of all recognized key codes, modeled after crossterm's `KeyCode` and tcell's `Key`:

```
enum Key `public {
  Char(char ch),       // printable character
  Enter,
  Escape,
  Backspace,
  Tab,
  BackTab,             // Shift+Tab
  Delete,
  Insert,
  Home,
  End,
  PageUp,
  PageDown,
  Up,
  Down,
  Left,
  Right,
  F(u8 number),        // F1..F24
  Null,                // Ctrl+Space / Ctrl+@
}
```

### `Modifier`

```
type Modifier `public {
  bool has_ctrl `value;
  bool has_alt `value;
  bool has_shift `value;
  bool has_super `value;

  none() Self `factory {
    return Self(has_ctrl: false, has_alt: false, has_shift: false, has_super: false);
  }
}
```

### `MouseEvent`

```
type MouseEvent `public {
  MouseAction action `value;
  MouseButton button `value;
  Point[int] pos `value;     // 0-indexed screen position
  Modifier modifiers `value;
}
```

### `MouseButton` / `MouseAction`

```
enum MouseButton `public {
  Left,
  Right,
  Middle,
  None,   // for move events with no button held
}

enum MouseAction `public {
  Press,
  Release,
  Move,        // mouse motion (requires mouse enabled)
  ScrollUp,
  ScrollDown,
  ScrollLeft,
  ScrollRight,
}
```

### `CursorShape`

```
enum CursorShape `public {
  Block,
  Underline,
  Bar,
  BlinkingBlock,
  BlinkingUnderline,
  BlinkingBar,
}
```

---

## Event Loop Patterns

### Simple blocking loop

```
use term;

main()! {
  use screen := term.Screen.open();
  screen.clear();

  bool running = true;
  while running {
    screen.write(Point(0, 0), "Press 'q' to quit", style: term.Style.default());
    screen.show();

    term.Event event = screen.poll_event();
    match event {
      term.Event.Key(k) => {
        match k.code {
          term.Key.Char('q') => { running = false; },
          term.Key.Escape    => { running = false; },
          _ => {},
        }
      },
      term.Event.Resize(_) => {
        screen.show(full: true);  // full redraw after resize
      },
      _ => {},
    }
  }
}
```

### Channel-based event loop (async with goroutines)

Separates input from rendering using Promise's concurrency primitives — this mirrors
how tcell recommends using `PollEvent` inside a goroutine.

```
use term;

main()! {
  use screen := term.Screen.open();
  screen.enable_mouse();

  // events() spawns a reader goroutine, returns channel[Event]
  channel[term.Event] events = screen.events();

  // Application state
  cursor := Point(0, 0);

  for event in events {
    match event {
      term.Event.Key(k) => {
        match k.code {
          term.Key.Char('q') => { break; },
          term.Key.Up        => { if cursor.y > 0 { cursor.y -= 1; } },
          term.Key.Down      => { cursor.y += 1; },
          term.Key.Left      => { if cursor.x > 0 { cursor.x -= 1; } },
          term.Key.Right     => { cursor.x += 1; },
          _ => {},
        }
      },
      term.Event.Mouse(m) => {
        if m.action is term.MouseAction.Press {
          cursor = m.pos;
        }
      },
      term.Event.Resize(_) => {
        screen.show(full: true);
      },
      _ => {},
    }

    // Redraw
    screen.clear();
    screen.write(Point(0, 0), "Pos: ({cursor.x}, {cursor.y})", style: term.Style.default());
    screen.set_cursor(cursor);
    screen.show();
  }
}
```

### Select with timers (animation / polling)

```
use term;

main()! {
  use screen := term.Screen.open();
  channel[term.Event] events = screen.events();
  tick := channel[bool](capacity: 1);

  // Tick every 100ms
  go {
    for {
      os.sleep(100);
      tick.send(true);
    }
  };

  int frame = 0;

  for {
    select {
      event := <-events:
        match event {
          term.Event.Key(k) => {
            if k.code is term.Key.Char('q') { return; }
          },
          _ => {},
        }
      _ := <-tick:
        frame += 1;
        screen.clear();
        screen.write(Point(0, 0), "Frame: {frame}", style: term.Style.default());
        screen.show();
    }
  }
}
```

### Subprocess handoff (suspend / resume)

Temporarily yields the terminal to a subprocess — the pattern used by `git commit`
opening `$EDITOR`, or any TUI that shells out to another interactive program.

```
use term;
use os;

main()! {
  use screen := term.Screen.open();

  screen.write(Point(0, 0), "Press 'e' to open editor, 'q' to quit",
    style: term.Style.default());
  screen.show();

  for {
    event := screen.poll_event();
    match event {
      term.Event.Key(k) => {
        match k.code {
          term.Key.Char('e') => {
            // Hand terminal to $EDITOR
            screen.suspend();
            os.execute("vim", "/tmp/note.txt");
            screen.resume();   // re-enters raw mode, full redraw
          },
          term.Key.Char('q') => { return; },
          _ => {},
        }
      },
      _ => {},
    }
  }
}
```

---

## Comparison: How the Primitives Map

| Concept              | curses (C/Python)       | crossterm (Rust)                   | tcell (Go)                      | **Promise term**                  |
| :------------------- | :---------------------- | :--------------------------------- | :------------------------------ | :-------------------------------- |
| Init terminal        | `initscr()`             | `enable_raw_mode()` + `execute!()` | `screen.Init()`                 | `Screen.open()`                   |
| Teardown             | `endwin()`              | `disable_raw_mode()`               | `screen.Fini()`                 | `screen.close()` / `use` auto    |
| Alt screen           | N/A (default)           | `EnterAlternateScreen` cmd         | Automatic in `Init()`           | Automatic in `open()` (opt-out)   |
| Raw mode             | `cbreak()` / `raw()`   | `enable_raw_mode()`                | Automatic in `Init()`           | Automatic in `open()` (opt-out)   |
| Set cell             | `mvaddch(y, x, ch)`    | `queue!(MoveTo, Print)`            | `screen.SetContent(x, y, ...)`  | `screen.set_cell(pos, ...)`       |
| Write string         | `mvaddstr(y, x, s)`    | `queue!(MoveTo, Print)`            | Custom helper over SetContent   | `screen.write(pos, ...)`          |
| Refresh              | `refresh()`             | `stdout.flush()`                   | `screen.Show()`                 | `screen.show()`                   |
| Full redraw          | `touchwin()` + refresh  | N/A (manual)                       | `screen.Sync()`                 | `screen.show(full: true)`         |
| Clear                | `clear()`               | `Clear(ClearType::All)`            | `screen.Clear()`                | `screen.clear()`                  |
| Get size             | `getmaxyx()`            | `terminal::size()?`                | `screen.Size()`                 | `screen.size`                     |
| Poll event           | `getch()`               | `event::read()?`                   | `screen.PollEvent()`            | `screen.poll_event()`             |
| Non-blocking poll    | `nodelay()` + `getch()` | `event::poll()` + `read()`        | `HasPendingEvent()` + poll      | `screen.try_poll_event()`         |
| Style / color        | `COLOR_PAIR` + `attron`  | `SetForegroundColor`, `SetAttribute` | `tcell.Style` builder        | `Style.default().foreground(...).bold()` |
| Mouse                | `mousemask()`           | `EnableMouseCapture`               | `screen.EnableMouse()`          | `screen.enable_mouse()`          |
| Cursor visibility    | `curs_set()`            | `Hide`/`Show` cursor cmds         | `ShowCursor` / `HideCursor`     | `set_cursor()` / `hide_cursor()`  |
| Subprocess handoff   | `endwin()` / `refresh()`| Manual save/restore                | `Suspend()` / `Resume()`        | `suspend()` / `resume()`          |
| TTY detection        | `isatty()`              | `IsTerminal` trait                 | `term.IsTerminal(fd)`           | `term.is_terminal()`           |

---

## Full Example: Centered Message Box

```
use term;

main()! {
  use screen := term.Screen.open();

  string message = "Press any key to continue...";
  term.Style box_style = term.Style.default()
    .foreground(term.Color.White)
    .background(term.Color.Blue)
    .bold();
  term.Style border_style = term.Style.default()
    .foreground(term.Color.BrightWhite)
    .background(term.Color.Blue);

  bool running = true;
  while running {
    sz := screen.size;

    // Box dimensions
    int box_w = message.len + 4;
    int box_h = 5;
    int x0 = (sz.width - box_w) / 2;
    int y0 = (sz.height - box_h) / 2;
    box := Rect(x0, y0, box_w, box_h);

    screen.clear();

    // Draw border
    screen.fill(box, ' ', style: border_style);

    // Top/bottom edges
    for x in x0..(x0 + box_w) {
      screen.set_cell(Point(x, y0), '─', style: border_style);
      screen.set_cell(Point(x, y0 + box_h - 1), '─', style: border_style);
    }

    // Left/right edges
    for y in y0..(y0 + box_h) {
      screen.set_cell(Point(x0, y), '│', style: border_style);
      screen.set_cell(Point(x0 + box_w - 1, y), '│', style: border_style);
    }

    // Corners
    screen.set_cell(Point(x0, y0), '┌', style: border_style);
    screen.set_cell(Point(x0 + box_w - 1, y0), '┐', style: border_style);
    screen.set_cell(Point(x0, y0 + box_h - 1), '└', style: border_style);
    screen.set_cell(Point(x0 + box_w - 1, y0 + box_h - 1), '┘', style: border_style);

    // Message text (centered in box)
    screen.write(Point(x0 + 2, y0 + 2), message, style: box_style);

    screen.hide_cursor();
    screen.show();

    term.Event event = screen.poll_event();
    match event {
      term.Event.Key(_) => { running = false; },
      term.Event.Resize(_) => { screen.show(full: true); },
      _ => {},
    }
  }
}
```

---

## Implementation Notes

### Platform Abstraction

Under the hood, `Screen.open()` would:

- **POSIX (Linux, macOS):** Save termios state, set raw mode via `tcsetattr`, write
  ANSI escape sequences for alternate screen (`\x1b[?1049h`), cursor control, mouse
  mode (`\x1b[?1000h`), etc. Read events by parsing stdin bytes as ANSI escape
  sequences.

- **Windows:** Use `SetConsoleMode` to disable line input and echo. Use the Console
  API (`ReadConsoleInput`) for events, or the newer virtual terminal sequences on
  Windows 10+. Alternate screen via `\x1b[?1049h` (VT mode) or console buffer
  switching.

### TTY Fallback

When `tty_fallback` is enabled (the default), `Screen.open()` checks whether
stdin and stdout refer to a terminal. If they don't — because the program is invoked
as `cat data.txt | myapp` or `myapp > log.txt` — it opens the controlling terminal
directly (`/dev/tty` on POSIX, `CONIN$`/`CONOUT$` on Windows) for both input and
output. This is the same strategy used by `less`, `git`, `vim`, and most interactive
pagers. If `tty_fallback` is set to `false`, `open()` raises an error when stdio
isn't a TTY, which is useful for tools that should fail early in non-interactive
environments.

### Cleanup Guarantees

The most dangerous bug in TUI programs is leaving the terminal in raw mode after a
crash. Promise's `use` binding must guarantee that `close()` runs even on unhandled
errors and panics — the same guarantee that Rust's `Drop` and Go's `defer` provide.
If Promise doesn't currently run `use` cleanup on panic, this is a prerequisite for
shipping the term module — a TUI library that can brick your terminal on any
unhandled error is unusable.

### Performance: Diff-Based Show

`show()` compares the "front buffer" (what's on screen) with the "back buffer" (what
the app wrote) and only emits escape sequences for changed cells. This is exactly how
tcell's `Show()` and ncurses' `refresh()` work. Calling `show(full: true)` bypasses
the diff and redraws everything — necessary after a `SIGWINCH` or subprocess corruption.

### Thread Safety

`Screen` methods that take `~this` (mutable borrow) are not concurrently callable by
design — Promise's borrow checker prevents it. The `events()` channel pattern lets a
reader goroutine own the polling while the main goroutine owns rendering, with the
channel as the safe boundary.

### Why Not a Streaming / Command API?

Crossterm uses a "command" pattern where you queue ANSI operations and flush. This is
flexible but error-prone — it's easy to forget cursor repositioning, leave style state
leaked, etc. The cell-buffer model (used by tcell, termbox, notcurses, and curses) is
higher-level, eliminates flicker via diff-based updates, and is much easier for agents
and beginners to reason about. A lower-level `term.raw_write(string)` escape hatch
could be added later for advanced use cases without compromising the primary API.

---

## Future Extensions (Out of Scope for v1)

These are explicitly *not* in the initial module but designed to layer on top:

- **`term.widgets`** — higher-level components (text input, list select, scrollable
  viewport, table, progress bar) built on `Screen`.
- **`term.layout`** — flexbox-like layout engine for splitting the screen into panes.
- **`term.Canvas`** — a sub-region / window abstraction (like curses `WINDOW` or
  tcell's off-screen drawing), enabling composable widgets.
- **Sixel / Kitty image protocol** — inline image rendering on supported terminals.
- **24-bit color detection** — query `COLORTERM` env var and degrade gracefully.
