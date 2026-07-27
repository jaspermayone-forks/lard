#!/usr/bin/env python3
"""Live progress for a lard consolidation run.

Usage: progress.py [DB_PATH]   (default: $LARD_DB or /tmp/lard-home/lard.db)
"""
import os
import sqlite3
import sys
import time

db_path = sys.argv[1] if len(sys.argv) > 1 else os.getenv("LARD_DB", "/tmp/lard-home/lard.db")


def snapshot(db):
    total = db.execute("SELECT COUNT(*) FROM sessions").fetchone()[0]
    done = db.execute("SELECT COUNT(*) FROM extracted").fetchone()[0]
    facts = db.execute("SELECT COUNT(*) FROM facts").fetchone()[0]
    subs = db.execute("SELECT COUNT(*) FROM subjects").fetchone()[0]
    return total, done, facts, subs


def main():
    db = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    start = time.time()
    start_done = None
    printed = False
    try:
        while True:
            total, done, facts, subs = snapshot(db)
            if start_done is None:
                start_done = done
            pct = done / total * 100 if total else 0
            bar = "█" * int(pct / 2) + "░" * (50 - int(pct / 2))
            elapsed = time.time() - start
            rate = (done - start_done) / elapsed if elapsed > 0 else 0
            eta = "—"
            if rate > 0 and done < total:
                secs = (total - done) / rate
                eta = f"{int(secs // 60)}m{int(secs % 60):02d}s"
            # Move up 2 lines and redraw in place (after the first frame).
            if printed:
                sys.stdout.write("\033[2A")
            sys.stdout.write(f"\r[{bar}] {pct:.0f}%\033[K\n")
            sys.stdout.write(
                f"\r{done}/{total} sessions · {facts} facts · {subs} subjects · eta {eta}\033[K\n"
            )
            sys.stdout.flush()
            printed = True
            if done >= total and total > 0:
                sys.stdout.write("extract complete; synthesis runs next\n")
                return
            time.sleep(3)
    except KeyboardInterrupt:
        sys.stdout.write("\n")


if __name__ == "__main__":
    main()
