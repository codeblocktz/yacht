/**
 * The request timeline above an app's HTTP log.
 *
 * TypeScript, bundled by esbuild into a file that is committed. Building and
 * running Yacht need neither Node nor this source — which is the same bargain
 * the stylesheet strikes, and it is what keeps a self-hoster able to run the
 * binary without a toolchain.
 */
import { areaY, colorLegend, defineChart, lineY, mountChart, rect, ruleY } from '@tanstack/charts'
import { tooltip } from '@tanstack/charts/tooltip'
import { scaleLinear, scaleOrdinal, scaleUtc } from 'd3-scale'

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

/**
 * The same columns over a project's log: lines, and the errors among them.
 * Lines are neutral rather than green, because a line is not a success the
 * way a 200 is — only the errors are a colour, and they are red.
 */
const LOG_BANDS: readonly { key: BandKey; label: string; colour: string }[] = [
  { key: 'ok', label: 'Lines', colour: 'var(--chart-lines)' },
  { key: 'err', label: 'Errors', colour: 'var(--chart-err)' },
]

/**
 * A time axis label, in UTC as every time on these pages is: the clock for a
 * range of hours, the day for a range of days. d3's default switches between
 * "05 PM" and "05:15" on one axis, which reads as two different clocks.
 */
function timeTick(spanMs: number): (d: Date) => string {
  const pad = (n: number) => String(n).padStart(2, '0')
  if (spanMs > 36 * 3600 * 1000) {
    const months = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']
    return (d) => `${months[d.getUTCMonth()]} ${d.getUTCDate()}`
  }
  if (spanMs < 10 * 60 * 1000) {
    // Minutes repeat across a few minutes' ticks; the seconds tell them apart.
    return (d) => `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`
  }
  return (d) => `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`
}

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
function toBlocks(buckets: readonly Bucket[], bands: typeof BANDS): Block[] {
  const blocks: Block[] = []
  for (const b of buckets) {
    const from = new Date(b.at)
    const to = new Date(b.to)
    let base = 0
    for (const band of bands) {
      const count = b[band.key] ?? 0
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
  const logs = el.dataset.bands === 'logs'
  const bands = logs ? LOG_BANDS : BANDS
  const blocks = toBlocks(readBuckets(el), bands)
  if (blocks.length === 0) return

  // One mark per status class. Each block already carries where it starts and
  // ends on both axes, so the marks need no knowledge of each other.
  const marks = bands.map((band) =>
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

  const first = blocks[0]?.from.getTime() ?? 0
  const last = blocks[blocks.length - 1]?.to.getTime() ?? 0
  const definition = defineChart({
    marks,
    x: { scale: scaleUtc, nice: false, axis: { ticks: { format: timeTick(last - first) } } },
    // Whole requests only. A count axis labelled 0.5 describes something that
    // cannot happen.
    y: { scale: scaleLinear, nice: true, grid: true, ticks: 3 },
    tooltip,
  })

  el.textContent = ''
  mountChart(el, {
    definition,
    height: logs ? 84 : 96,
    initialWidth: el.clientWidth || 640,
    ariaLabel: logs ? 'Log lines over time, with errors' : 'Requests over time, by response status',
    ariaDescription: logs
      ? 'Stacked columns counting the log lines in each time bucket, with the errors among them on top.'
      : 'Stacked columns counting successful, client-error and server-error ' +
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

// ---------------------------------------------------------------------------
// Series charts: the Observability page's blocks, and an app's Metrics tab.

/** One panel, as the server read it from the metrics store. */
interface Panel {
  key: string
  title: string
  unit: Unit
  stacked?: boolean
  start: string
  end: string
  domain: string[] | null
  series: { name: string; points: [number, number][] }[] | null
}

type Unit = 'cores' | 'bytes' | 'bytes/s' | 'req/s' | 'ratio' | 'seconds'

/** One drawn point: a series' value at a time. */
interface Sample {
  at: Date
  value: number
  series: string
}

/**
 * Colour by the job it does. Services are identities, so they take the
 * categorical slots in the project's fixed order — the same service is the
 * same colour on every block. Status classes are states, so they take the
 * status colours the HTTP timeline already uses. Percentiles are ordered, so
 * they are one hue from light to dark.
 */
const STATUS_COLOURS: Record<string, string> = {
  '2xx': 'var(--chart-ok)',
  '3xx': 'var(--chart-info)',
  '4xx': 'var(--chart-warn)',
  '5xx': 'var(--chart-err)',
}
const PERCENTILE_COLOURS = ['var(--chart-seq-1)', 'var(--chart-seq-2)', 'var(--chart-seq-3)', 'var(--chart-seq-4)']
const SERIES_SLOTS = 8

function colourFor(panel: Panel, name: string, index: number): string {
  if (panel.key === 'requests') return STATUS_COLOURS[name] ?? 'var(--chart-lines)'
  if (panel.key === 'latency') return PERCENTILE_COLOURS[index] ?? 'var(--chart-seq-4)'
  // A ninth service folds back onto the slots rather than inventing a hue;
  // the legend still names each one.
  return `var(--series-${(index % SERIES_SLOTS) + 1})`
}

/** Writes a value in the panel's unit, for the axis and the tooltip. */
function formatValue(unit: Unit, v: number): string {
  if (v === 0) return unit === 'ratio' ? '0%' : '0'
  switch (unit) {
    case 'cores':
      return v < 1 ? `${Number((v * 1000).toFixed(v < 0.01 ? 1 : 0))}m` : String(Number(v.toFixed(2)))
    case 'bytes':
      return bytes(v)
    case 'bytes/s':
      return `${bytes(v)}/s`
    case 'req/s':
      // Trailing zeros dropped: an axis reading 5.00 claims a precision a
      // tick does not have.
      return v < 10 ? String(Number(v.toFixed(v < 0.1 ? 3 : 2))) : Math.round(v).toString()
    case 'ratio':
      return `${(v * 100).toFixed(v < 0.1 ? 1 : 0)}%`
    case 'seconds':
      return v < 1 ? `${Math.round(v * 1000)} ms` : `${v.toFixed(2)} s`
  }
}

function bytes(v: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || (i === 0 && v >= 10) ? Math.round(v) : String(Number(v.toFixed(1)))} ${units[i]}`
}

function readPanel(el: HTMLElement): Panel | null {
  const script = el.dataset.source ? document.getElementById(el.dataset.source) : null
  if (!script) return null
  try {
    return JSON.parse(script.textContent ?? 'null') as Panel | null
  } catch {
    return null
  }
}

/**
 * Draws one block. Nothing is drawn for a panel with no points, which keeps
 * the empty state the server rendered in its place.
 */
function drawSeries(el: HTMLElement): void {
  const panel = readPanel(el)
  if (!panel?.series) return
  const order = panel.domain && panel.domain.length > 0 ? panel.domain : panel.series.map((s) => s.name)
  // Every named series keeps its slot even when it has no data in this range,
  // so filtering never repaints the services that remain.
  const names = [...order, ...panel.series.map((s) => s.name).filter((n) => !order.includes(n))]
  const samples: Sample[] = []
  for (const s of panel.series) {
    for (const [ms, value] of s.points) samples.push({ at: new Date(ms), value, series: s.name })
  }
  if (samples.length === 0) return

  // The legend names only what is drawn, but each keeps the colour of its
  // place in the whole list, so a service is the same colour on every block.
  const present = names.filter((n) => samples.some((d) => d.series === n))
  const colour = scaleOrdinal<string, string>()
    .domain(present)
    .range(present.map((n) => colourFor(panel, n, names.indexOf(n))))

  // A zero baseline on every block: use, traffic and time are all measured
  // from nothing, and an axis that starts at the lowest reading turns a
  // flat 120m into a cliff.
  const base = ruleY([0], { stroke: 'var(--border)' })
  const marks = panel.stacked
    ? [base, areaY(samples, { x: 'at', y: 'value', z: 'series', fillOpacity: 0.85 })]
    : [base, lineY(samples, { x: 'at', y: 'value', z: 'series', strokeWidth: 2 })]

  const format = (v: number) => formatValue(panel.unit, v)
  const definition = defineChart({
    marks,
    x: {
      scale: scaleUtc().domain([new Date(panel.start), new Date(panel.end)]),
      nice: false,
      axis: { ticks: { count: 4, format: timeTick(new Date(panel.end).getTime() - new Date(panel.start).getTime()) } },
    },
    y: {
      scale: scaleLinear,
      nice: true,
      grid: true,
      axis: { ticks: { count: 3, format } },
    },
    // A legend whenever there is more than one series, so identity is never
    // colour alone; a single series is named by the block's title.
    color: present.length > 1 ? { scale: colour, legend: colorLegend({}) } : { scale: colour },
    focus: 'group-x',
    tooltip,
  })

  el.textContent = ''
  mountChart(el, {
    definition,
    height: 150,
    initialWidth: el.clientWidth || 480,
    ariaLabel: `${panel.title} over time`,
    ariaDescription: `${present.length} series: ${present.join(', ')}.`,
  })
}

/** Draws every series chart on the page that has not been drawn yet. */
export function mountSeries(root: ParentNode = document): void {
  root.querySelectorAll<HTMLElement>('[data-series-chart]').forEach((el) => {
    if (el.dataset.drawn === 'true') return
    el.dataset.drawn = 'true'
    drawSeries(el)
  })
}

// The panel arrives by htmx swap, not with the page, so a single call on load
// would draw nothing. Both are wired: load for a full page render, and htmx's
// settle for every swap after it.
document.addEventListener('DOMContentLoaded', () => {
  mountTimelines()
  mountSeries()
})
document.body?.addEventListener('htmx:afterSettle', (event) => {
  const target = (event as CustomEvent).detail?.target
  // An outerHTML swap's target is the element it replaced, no longer in the
  // page; the charts it brought are found from the document instead.
  const root = target instanceof Element && target.isConnected ? target : document
  mountTimelines(root)
  mountSeries(root)
})
