/**
 * A keyed list reconciler, small enough to read in one sitting.
 *
 * The conversation is an append-only log that is occasionally rewritten: history
 * prepends, a resync replaces it wholesale, and — the case this exists for — a
 * tool card changes *in place* when its result lands. The previous implementation
 * compared keys alone, so a row whose key stayed the same was never re-rendered:
 * a running tool card kept saying "running" and never showed its output until the
 * page was reloaded.
 *
 * The contract that fixes it is small: **a row's data object is replaced only
 * when the row's content changed**, and this list re-renders exactly when the
 * object identity changes. Producers get that for free by building new rows only
 * for the items that moved; consumers get in-place updates instead of remounts,
 * which is also what preserves a reader's scroll position and an open card.
 *
 * The walk is the ordinary one for an append-only list: keep the common prefix,
 * drop the mismatched tail, mount what is new. It is O(changed rows), and the
 * common case — one frame appended — touches one node.
 */

/** A mounted row: its node, and the optional hooks to keep it current. */
export interface RowHandle<T> {
  readonly node: HTMLElement;
  /** Re-render from new data that describes the same row. */
  update?(data: T): void;
  /** Release anything the row holds — timers, listeners, observers. */
  dispose?(): void;
}

/**
 * How to build and identify one row.
 *
 * `key` is the row's identity across reconciliations; the row object itself is
 * its content, so an unchanged row that is passed through again by reference is
 * a row that does not need re-rendering. Producers keep that promise by reusing
 * the previous object whenever nothing about it changed.
 */
export interface RowRenderer<T> {
  key(row: T): string;
  mount(row: T): RowHandle<T>;
}

/** What a reconciliation did, for a caller that has to keep a scroll position. */
export interface RowChange {
  /** Anything at all was mounted, updated, or removed. */
  readonly changed: boolean;
  /**
   * Rows appeared *above* the first row that was on screen — which is what
   * "load older" does. A caller that was not pinned to the end has to add the
   * new height to its scroll offset, or the reader's place jumps.
   */
  readonly prepended: boolean;
}

interface Mounted<T> {
  readonly key: string;
  row: T;
  readonly handle: RowHandle<T>;
}

/**
 * Owns one host element's children.
 *
 * The host must be dedicated to this list: reconciliation assumes the children it
 * finds are the ones it mounted.
 */
export class RowList<T> {
  readonly #host: HTMLElement;
  readonly #renderer: RowRenderer<T>;
  #mounted: Mounted<T>[] = [];

  constructor(host: HTMLElement, renderer: RowRenderer<T>) {
    this.#host = host;
    this.#renderer = renderer;
  }

  /** How many rows are currently mounted. Used by callers that page history. */
  get size(): number {
    return this.#mounted.length;
  }

  /**
   * Reconciles the host against `rows`.
   *
   * The returned change says whether to follow the new content and whether the
   * reader's place needs holding, so a caller never has to measure the DOM to
   * find out what happened.
   */
  set(rows: readonly T[]): RowChange {
    let changed = false;
    const firstBefore = this.#mounted[0]?.key ?? null;

    // The prefix whose keys line up: keep the nodes, refresh only the ones whose
    // data object was replaced.
    let index = 0;
    while (index < rows.length && index < this.#mounted.length) {
      const entry = this.#mounted[index];
      const row = rows[index];
      if (entry === undefined || row === undefined || entry.key !== this.#renderer.key(row)) break;
      if (entry.row !== row) {
        entry.handle.update?.(row);
        entry.row = row;
        changed = true;
      }
      index += 1;
    }

    // Everything the prefix did not account for goes: a rebuilt feed, a shorter
    // one, or a row replaced by a different row.
    while (this.#mounted.length > index) {
      const entry = this.#mounted.pop();
      if (entry === undefined) break;
      entry.handle.dispose?.();
      entry.handle.node.remove();
      changed = true;
    }

    // Then mount what is new, in order. The append case ends up here.
    for (; index < rows.length; index += 1) {
      const row = rows[index];
      if (row === undefined) continue;
      const handle = this.#renderer.mount(row);
      this.#host.appendChild(handle.node);
      this.#mounted.push({ key: this.#renderer.key(row), row, handle });
      changed = true;
    }

    const first = rows[0] === undefined ? null : this.#renderer.key(rows[0]);
    const prepended =
      firstBefore !== null && first !== firstBefore && rows.some((row) => this.#renderer.key(row) === firstBefore);
    return { changed, prepended };
  }

  /** Unmounts everything. Used when the view that owns the list goes away. */
  clear(): void {
    for (const entry of this.#mounted) {
      entry.handle.dispose?.();
      entry.handle.node.remove();
    }
    this.#mounted = [];
  }
}
