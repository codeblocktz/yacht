/**
 * The request timeline above an app's HTTP log.
 *
 * TypeScript, bundled by esbuild into a file that is committed. Building and
 * running Yacht need neither Node nor this source — which is the same bargain
 * the stylesheet strikes, and it is what keeps a self-hoster able to run the
 * binary without a toolchain.
 */
import { defineChart, mountChart, rect } from '@tanstack/charts'
import { tooltip } from '@tanstack/charts/tooltip'
import { scaleLinear, scaleUtc } from 'd3-scale'

/** One column, as the server counted it. */
interface Bucket {
  at: string
  to: string
  ok: number
  warn: number
  err: number
}

/** One drawn block: a bucket's share of one status class, already stacked. */
interface Block {
  from: Date
  to: Date
  base: number
  top: number
  band: BandKey
  count: number
}

type BandKey = 'ok' | 'warn' | 'err'

/**
 * The three status classes, in the order they stack.
 *
 * Failures on top and in a fixed order. A stack sorted by count would move
 * them around as traffic changes, and finding failures is the whole reason to
 * look at this.
 */
const BANDS: readonly { key: BandKey; label: string; colour: string }[] = [
  { key: 'ok', label: '2xx/3xx', colour: 'var(--chart-ok)' },
  { key: 'warn', label: '4xx', colour: 'var(--chart-warn)' },
  { key: 'err', label: '5xx', colour: 'var(--chart-err)' },
]

/** Reads the counts the server embedded next to the chart. */
function readBuckets(el: HTMLElement): Bucket[] {
  const id = el.dataset.source
  if (!id) return []
  const script = document.getElementById(id)
  if (!script) return []
  try {
    const parsed: unknown = JSON.parse(script.textContent ?? '[]')
    return Array.isArray(parsed) ? (parsed as Bucket[]) : []
  } catch {
    // A malformed payload leaves the panel without a chart, which is what the
    // markup already shows. Throwing here would take the log list with it.
    return []
  }
}

/**
 * Turns buckets into stacked blocks.
 *
 * Stacked here rather than by the chart, because each band is drawn by its own
 * mark — rect takes a plain fill rather than a colour channel — and three
 * independent marks cannot agree on a stack between them.
 */
function toBlocks(buckets: readonly Bucket[]): Block[] {
  const blocks: Block[] = []
  for (const b of buckets) {
    const from = new Date(b.at)
    const to = new Date(b.to)
    let base = 0
    for (const band of BANDS) {
      const count = b[band.key]
      // Empty bands are skipped. A zero-height block is invisible and still
      // costs a mark per bucket per band.
      if (count <= 0) continue
      blocks.push({ from, to, base, top: base + count, band: band.key, count })
      base += count
    }
  }
  return blocks
}

/**
 * Draws one timeline.
 *
 * Returns without drawing when there is nothing to draw, so a panel with no
 * requests keeps the empty state the server rendered rather than gaining an
 * empty pair of axes.
 */
function draw(el: HTMLElement): void {
  const blocks = toBlocks(readBuckets(el))
  if (blocks.length === 0) return

  // One mark per status class. Each block already carries where it starts and
  // ends on both axes, so the marks need no knowledge of each other.
  const marks = BANDS.map((band) =>
    rect(
      blocks.filter((d) => d.band === band.key),
      {
        id: band.key,
        x1: 'from',
        x2: 'to',
        y1: 'base',
        y2: 'top',
        fill: band.colour,
        inset: 0.5,
        radius: 1,
      },
    ),
  )

  const definition = defineChart({
    marks,
    x: { scale: scaleUtc, nice: false },
    // Whole requests only. A count axis labelled 0.5 describes something that
    // cannot happen.
    y: { scale: scaleLinear, nice: true, grid: true, ticks: 3 },
    tooltip,
  })

  el.textContent = ''
  mountChart(el, {
    definition,
    height: 96,
    initialWidth: el.clientWidth || 640,
    ariaLabel: 'Requests over time, by response status',
    ariaDescription:
      'Stacked columns counting successful, client-error and server-error ' +
      'responses in each time bucket.',
  })
}

/** Draws every timeline on the page that has not been drawn yet. */
export function mountTimelines(root: ParentNode = document): void {
  const els = root.querySelectorAll<HTMLElement>('[data-http-chart]')
  els.forEach((el) => {
    if (el.dataset.drawn === 'true') return
    el.dataset.drawn = 'true'
    draw(el)
  })
}

// The panel arrives by htmx swap, not with the page, so a single call on load
// would draw nothing. Both are wired: load for a full page render, and htmx's
// settle for every swap after it.
document.addEventListener('DOMContentLoaded', () => mountTimelines())
document.body?.addEventListener('htmx:afterSettle', (event) => {
  const target = (event as CustomEvent).detail?.target
  mountTimelines(target instanceof Element ? target : document)
})
