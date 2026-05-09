package main

import "fmt"

func printHelp() {
	fmt.Print(`Promise — statically-typed language with Go-like concurency and Rust-like ownership.
Compiler: promise build file.pr | Run: promise run file.pr | Exec: promise exec 'print_line("hi")'

=== Quick Start ===

  // hello.pr — minimal program
  main() {
    print_line("Hello, world!");
  }

  // failable main — errors auto-propagate with bare calls
  use io;

  main!() {
    io.File f = io.open("data.txt");  // auto-propagates on failure
    string line = f.read_line();
    print_line(line);
  }

  // struct with named-arg constructor and methods
  type Point {
    f64 x;
    f64 y;

    distance(&this) f64 {
      return sqrt(this.x * this.x + this.y * this.y);
    }
  }

  // enum with match
  enum Shape {
    Circle(f64 radius),
    Rect(f64 w, f64 h),

    area(&this) f64 {
      return match this {
        Circle(r) => 3.14159 * r * r,
        Rect(w, h) => w * h,
      };
    }
  }

=== Key Differences from Familiar Languages ===

  Failable functions:  name!() marks a function that can fail.
  Error handling:      bare call = auto-propagate in ! functions.
                       call()?^ = explicit propagate (like Rust's ?).
                       call()?! = panic on error (prototyping only).
                       call()? e { fallback } = recover with handler.
  Ownership:           string and user types are move-by-default.
                       &x borrows immutably, ~x moves ownership.
                       No &/~ at call sites — compiler auto-borrows.
  Constructors:        named args: Point(x: 1.0, y: 2.0).
  Getters:             property syntax: v.len not v.len().
  String interpolation: "hello {name}" (braces, not $).
  Mutability:          ~this for mutating methods. Variables immutable by default.
  Modules:             use io; / use json; for catalog modules.
                       std is auto-imported (print_line, Channel, Map, etc.).

=== Available Modules ===

  Auto-imported (std):  print_line, Vector/T[], Map/map[K,V], Set, Channel,
                        string, int, f64, bool, char, error, assert,
                        Range/../.., Iterator, Builder, Duration, Random
  Catalog (use X;):     io, json, os, path, math, strings, time, console, http

  Discover module APIs: promise doc <module>  (e.g., promise doc io)

=== Discovery Commands ===

  promise help            This output
  promise guide           Full language reference (~800 lines, pipe into LLM context)
  promise examples        Browse and run example programs
  promise doc <module>    API docs for a module (e.g., promise doc io, promise doc std.vector)
  promise version         Compiler version
  promise build file.pr   Compile to executable
  promise run file.pr     Compile and run
  promise test file.pr    Run tests
  promise exec '<code>'   Run inline code (failable main, ?^ works)
`)
}
