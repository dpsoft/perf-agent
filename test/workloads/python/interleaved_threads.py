#!/usr/bin/env python3
"""Two Python segments separated by a C-extension frame, on a NON-MAIN thread.

This is the fixture the CPython frame walker is tested against, and every
part of its shape is load-bearing:

  * The work runs on a spawned thread and the main thread only joins. The
    walker reaches a PyThreadState through the pthread TSD slot named by
    CPython's autoTSSkey -- there is no other way to find the state of the
    thread that was sampled. A single-threaded fixture would pass against a
    walker that only ever found the main thread's state, leaving the whole
    mechanism unexercised.

  * `sorted(items, key=key_fn)` puts a C extension frame BETWEEN two Python
    segments: list.sort() is C code that calls back into the interpreter for
    every key. _PyInterpreterFrame.previous links run straight through that
    boundary, so a walker that restarts from tstate->current_frame at the
    second eval-loop frame re-pushes the inner segment underneath the outer
    one's native position -- duplicated frames in a plausible order. The
    fixture exists to make that failure visible.

  * The CODE lines below print id(f.__code__), which in CPython is the code
    object's address -- the exact word the walker pushes into the sample as
    a Python frame. Python frames are unsymbolized in this slice, so this is
    what lets a test assert WHICH functions were walked rather than merely
    that something Python-shaped appeared.

Stack shape while a sample lands inside leaf(), leaf-ward to root-ward:

    <native: eval loop>        _PyEval_EvalFrameDefault
    leaf, inner, key_fn        inner Python segment
    <native: list_sort_impl, builtin_sorted, ...>
    <native: eval loop>        _PyEval_EvalFrameDefault
    middle, outer, worker      outer Python segment
    <native: thread bootstrap>

Usage: interleaved_threads.py <seconds>
"""
import sys
import threading
import time

# Named functions, one per stack level, so the assertions can speak about a
# specific frame rather than "some Python frame". Nothing here is recursive:
# each code object must appear AT MOST ONCE in any one sample, which is what
# makes duplicated segments detectable.


def leaf(x):
    """Where the samples land. Sized so the key phase dominates the sort."""
    total = 0
    for i in range(300):
        total += (x * i) % 7
    return total


def inner(x):
    return leaf(x)


def key_fn(x):
    """Called by list.sort() -- C code re-entering the interpreter."""
    return inner(x)


def middle(items):
    """Enters C. sorted() calls key_fn once per item before comparing."""
    return sorted(items, key=key_fn)


def outer(items):
    return middle(items)


def worker(deadline):
    items = list(range(64))
    while time.time() < deadline:
        outer(items)


def main():
    duration = float(sys.argv[1]) if len(sys.argv) > 1 else 10.0

    # id(code) is the code object's address in this process; the walker
    # reports exactly this word. Printed before the worker starts so a
    # profiler attaching later still reads a complete list.
    for name in ("leaf", "inner", "key_fn", "middle", "outer", "worker"):
        print("CODE %s %#x" % (name, id(globals()[name].__code__)), flush=True)

    t = threading.Thread(
        target=worker, args=(time.time() + duration,), name="pyframes-worker"
    )
    t.start()
    print("READY", flush=True)
    # The main thread executes no Python bytecode from here on: every Python
    # frame an on-CPU profile can see belongs to the worker thread.
    t.join()


if __name__ == "__main__":
    main()
