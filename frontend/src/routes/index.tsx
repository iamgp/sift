import { createFileRoute } from '@tanstack/react-router'
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import type { ColumnDef } from '@tanstack/react-table'
import { useVirtualizer } from '@tanstack/react-virtual'
import { useDeferredValue, useEffect, useMemo, useRef, useState } from 'react'
import type { Dispatch, SetStateAction } from 'react'
import type { EChartsType } from 'echarts'
import {
  AlertCircle,
  Filter as FilterIcon,
  PanelRight,
  PlayCircle,
  Search as SearchIcon,
  Table2,
} from 'lucide-react'

type InferredFields = {
  timestamp: string
  level: string
  message: string
  source: string
  context: string
}

type LogRow = {
  line: number
  timestamp: string
  level: string
  message: string
  source?: string
  context?: string
  rawJSON: string
  prettyJSON: string
  searchText: string
  _parsedPayload?: unknown
}

type MalformedLine = { line: number; raw: string; error: string }

type IngestMetrics = {
  events: number
  eventsPerSec: number
  parseErrors: number
  droppedEvents: number
}

type BootstrapPayload = {
  path: string
  fields: InferredFields
  rows: LogRow[]
  malformed: MalformedLine[]
  metrics: IngestMetrics
  live: boolean
  retentionCap: number
}

type StreamEvent = {
  type: 'append' | 'malformed'
  row?: LogRow
  malformed?: MalformedLine
  metrics?: IngestMetrics
  fields?: InferredFields
}

type FilterClause =
  | { type: 'exists'; field: string }
  | { type: 'eq'; field: string; value: string }
  | { type: 'neq'; field: string; value: string }
  | { type: 'contains'; field: string; value: string }
  | { type: 'range'; field: string; low: string; high: string }

type HistogramBucket = {
  startMs: number
  endMs: number
  label: string
  info: number
  warn: number
  error: number
  other: number
  total: number
}

const UI_STATE_KEY = 'sift-ui-state-react-v1'
const MAX_PENDING_EVENTS = 5000
const MAX_MALFORMED = 5000
const MAX_BOOKMARKS = 10

export const Route = createFileRoute('/')({ component: SiftPage })

function SiftPage() {
  const [boot, setBoot] = useState<BootstrapPayload | null>(null)
  const [rows, setRows] = useState<LogRow[]>([])
  const [malformed, setMalformed] = useState<MalformedLine[]>([])
  const [metrics, setMetrics] = useState<IngestMetrics | null>(null)
  const [search, setSearch] = useState('')
  const [filtersInput, setFiltersInput] = useState('')
  const [selected, setSelected] = useState<LogRow | null>(null)
  const [livePaused, setLivePaused] = useState(false)
  const [pendingEvents, setPendingEvents] = useState<StreamEvent[]>([])
  const [bookmarks, setBookmarks] = useState<Array<{ search: string; filters: string }>>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [visibleCols, setVisibleCols] = useState<Record<string, boolean>>({
    timestamp: true,
    level: true,
    source: true,
    context: true,
    message: true,
    payload: true,
  })
  const viewportRef = useRef<HTMLDivElement | null>(null)
  const searchRef = useRef<HTMLInputElement | null>(null)
  const filtersRef = useRef<HTMLInputElement | null>(null)
  const malformedRef = useRef<HTMLDetailsElement | null>(null)
  const tablePanelRef = useRef<HTMLElement | null>(null)
  const deferredSearch = useDeferredValue(search)
  const deferredFiltersInput = useDeferredValue(filtersInput)

  useEffect(() => {
    try {
      const raw = window.localStorage.getItem(UI_STATE_KEY)
      if (!raw) return
      const parsed = JSON.parse(raw) as {
        search?: string
        filters?: string
        visibleCols?: Record<string, boolean>
        bookmarks?: Array<{ search?: string; filters?: string }>
      }
      if (typeof parsed.search === 'string') setSearch(parsed.search)
      if (typeof parsed.filters === 'string') setFiltersInput(parsed.filters)
      if (parsed.visibleCols && typeof parsed.visibleCols === 'object') {
        setVisibleCols((prev) => ({ ...prev, ...parsed.visibleCols }))
      }
      if (Array.isArray(parsed.bookmarks)) {
        setBookmarks(
          parsed.bookmarks
            .map((b) => ({ search: String(b?.search ?? ''), filters: String(b?.filters ?? '') }))
            .filter((b) => b.search || b.filters)
            .slice(0, MAX_BOOKMARKS),
        )
      }
    } catch {
      // ignore state restore failures
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      try {
        const res = await fetch('/api/bootstrap', { cache: 'no-store' })
        if (!res.ok) throw new Error(`bootstrap ${res.status}`)
        const data = (await res.json()) as BootstrapPayload
        if (cancelled) return
        setBoot(data)
        setRows(data.rows || [])
        setMalformed(data.malformed || [])
        setMetrics(data.metrics)
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(() => {
    try {
      window.localStorage.setItem(
        UI_STATE_KEY,
        JSON.stringify({ search, filters: filtersInput, visibleCols, bookmarks }),
      )
    } catch {
      // ignore quota issues
    }
  }, [search, filtersInput, visibleCols, bookmarks])

  useEffect(() => {
    if (!boot?.live) return
    const source = new EventSource('/events')
    source.onmessage = (evt) => {
      try {
        const payload = JSON.parse(evt.data) as StreamEvent
        if (livePaused) {
          setPendingEvents((prev) => {
            const next = [...prev, payload]
            return next.length > MAX_PENDING_EVENTS
              ? next.slice(next.length - MAX_PENDING_EVENTS)
              : next
          })
          return
        }
        applyStreamEvent(payload, boot, setRows, setMalformed, setMetrics)
      } catch {
        // ignore malformed event payload
      }
    }
    return () => source.close()
  }, [boot, livePaused])

  useEffect(() => {
    if (livePaused || pendingEvents.length === 0 || !boot) return
    setRows((prevRows) => {
      let nextRows = prevRows
      let nextMalformed = malformed
      let nextMetrics = metrics
      for (const ev of pendingEvents) {
        if (ev.metrics) nextMetrics = ev.metrics
        if (ev.type === 'append' && ev.row) {
          nextRows = [...nextRows, ev.row]
          const cap = boot.retentionCap || 500000
          if (nextRows.length > cap) nextRows = nextRows.slice(nextRows.length - cap)
        } else if (ev.type === 'malformed' && ev.malformed) {
          nextMalformed = [...nextMalformed, ev.malformed]
          if (nextMalformed.length > MAX_MALFORMED) nextMalformed = nextMalformed.slice(nextMalformed.length - MAX_MALFORMED)
        }
      }
      setMalformed(nextMalformed)
      setMetrics(nextMetrics)
      return nextRows
    })
    setPendingEvents([])
  }, [livePaused, pendingEvents, boot, malformed, metrics])

  const parsedFilters = useMemo(() => parseQueryClauses(deferredFiltersInput), [deferredFiltersInput])

  const filteredRows = useMemo(() => {
    const q = deferredSearch.trim().toLowerCase()
    const terms = q ? tokenizeQuery(q).map((t) => t.toLowerCase()) : []
    const clauses = parsedFilters
    if (!terms.length && !clauses.length) return rows
    return rows.filter((row) => {
      const hay = (row.searchText || `${row.line} ${row.timestamp} ${row.level} ${row.message} ${row.rawJSON}`).toLowerCase()
      if (!terms.every((t) => hay.includes(t))) return false
      if (!clauses.every((clause) => matchesClause(row, clause))) return false
      return true
    })
  }, [rows, deferredSearch, parsedFilters])

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null
      const tag = target?.tagName?.toLowerCase()
      const isEditable = !!target && (target.isContentEditable || tag === 'input' || tag === 'textarea' || tag === 'select')
      if (event.key === '/' && !event.metaKey && !event.ctrlKey && !event.altKey && !isEditable) {
        event.preventDefault()
        searchRef.current?.focus()
        searchRef.current?.select()
        return
      }
      if ((event.key === 'f' || event.key === 'F') && !event.metaKey && !event.ctrlKey && !event.altKey && !isEditable) {
        event.preventDefault()
        filtersRef.current?.focus()
        filtersRef.current?.select()
        return
      }
      if (event.key === 'Escape') {
        if (selected) {
          setSelected(null)
          return
        }
        if (target === searchRef.current && search) {
          event.preventDefault()
          setSearch('')
        }
        if (target === filtersRef.current && filtersInput) {
          event.preventDefault()
          setFiltersInput('')
        }
      }
      if ((event.key === 'p' || event.key === 'P') && !isEditable && boot?.live) {
        event.preventDefault()
        setLivePaused((v) => !v)
      }
      if (!isEditable && (event.key === 'j' || event.key === 'ArrowDown')) {
        event.preventDefault()
        setSelected((prev) => nextSelected(prev, filteredRows, 1))
      }
      if (!isEditable && (event.key === 'k' || event.key === 'ArrowUp')) {
        event.preventDefault()
        setSelected((prev) => nextSelected(prev, filteredRows, -1))
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [selected, search, filtersInput, boot?.live, filteredRows])

  const columns = useMemo<ColumnDef<LogRow>[]>(
    () => [
      { accessorKey: 'line', header: '#', size: 70, cell: (ctx) => <span className="mono dim">{String(ctx.getValue<number>())}</span> },
      { accessorKey: 'timestamp', header: 'Timestamp', size: 220 },
      {
        accessorKey: 'level',
        header: 'Level',
        size: 110,
        cell: (ctx) => <LevelBadge level={String(ctx.getValue<string>() || '')} />,
      },
      { accessorKey: 'source', header: 'Source', size: 140, cell: (ctx) => String(ctx.getValue<string>() || '-') },
      { accessorKey: 'context', header: 'Context', size: 140, cell: (ctx) => String(ctx.getValue<string>() || '-') },
      { accessorKey: 'message', header: 'Message', size: 420, cell: (ctx) => <span className="message-cell">{String(ctx.getValue<string>() || '-')}</span> },
      {
        id: 'payload',
        header: 'Payload',
        size: 100,
        cell: ({ row }) => (
          <button className="inline-details" onClick={(e) => { e.stopPropagation(); setSelected(row.original) }}>
            Details &gt;
          </button>
        ),
      },
    ],
    [],
  )

  const columnVisibility = useMemo(
    () => ({
      timestamp: visibleCols.timestamp,
      level: visibleCols.level,
      source: visibleCols.source,
      context: visibleCols.context,
      message: visibleCols.message,
      payload: visibleCols.payload,
    }),
    [visibleCols],
  )

  const table = useReactTable({
    data: filteredRows,
    columns,
    getCoreRowModel: getCoreRowModel(),
    state: { columnVisibility },
  })

  const rowModel = table.getRowModel().rows
  const virtualizer = useVirtualizer({
    count: rowModel.length,
    getScrollElement: () => viewportRef.current,
    estimateSize: () => 28,
    overscan: 20,
  })
  const virtualRows = virtualizer.getVirtualItems()

  const histogram = useMemo(() => buildHistogram(filteredRows), [filteredRows])

  if (loading) return <div className="screen"><div className="panel">Loading…</div></div>
  if (error) return <div className="screen"><div className="panel error">Failed to load: {error}</div></div>
  if (!boot) return null

  return (
    <div className="screen">
      <header className="topbar">
        <div className="brand"><span className="mark">SF</span><div><strong>Sift</strong><small>TanStack Start + TanStack Table</small></div></div>
        <div className="pills"><span>Local</span><span>{boot.live ? 'Live' : 'File'}</span><span>{(boot.retentionCap || rows.length).toLocaleString()} cap</span></div>
      </header>

      <div className="shell">
        <aside className="rail">
          <button className="railbtn active" aria-label="Search" onClick={() => searchRef.current?.focus()} title="Search">
            <SearchIcon size={14} strokeWidth={2} />
          </button>
          <button className="railbtn" aria-label="Filters" onClick={() => filtersRef.current?.focus()} title="Filters">
            <FilterIcon size={14} strokeWidth={2} />
          </button>
          <button className="railbtn" aria-label="Stream" onClick={() => boot.live && setLivePaused((v) => !v)} title="Stream">
            <PlayCircle size={14} strokeWidth={2} />
          </button>
          <button className="railbtn railbtn-danger" aria-label="Errors" onClick={() => malformedRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })} title="Errors">
            <AlertCircle size={14} strokeWidth={2} />
          </button>
          <button className="railbtn" aria-label="Details" onClick={() => setSelected((prev) => prev ?? filteredRows[0] ?? null)} title="Details">
            <PanelRight size={14} strokeWidth={2} />
          </button>
          <button className="railbtn" aria-label="Table" onClick={() => tablePanelRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })} title="Table">
            <Table2 size={14} strokeWidth={2} />
          </button>
        </aside>

        <main className="content">
          <section className="meta-grid">
            <div className="panel span-all"><strong>File:</strong> {boot.path}</div>
            <div className="panel"><strong>Rows:</strong> {rows.length.toLocaleString()}</div>
            <div className="panel"><strong>Visible:</strong> {filteredRows.length.toLocaleString()}</div>
            <div className="panel"><strong>Errors:</strong> {malformed.length.toLocaleString()}</div>
            <div className="panel"><strong>Ingest:</strong> {metrics ? `${metrics.eventsPerSec.toFixed(1)} eps` : '0.0 eps'}</div>
          </section>

          <section className="panel controls">
            <div className="search-grid">
              <div className="search-wrap">
                <label>Search</label>
                <input ref={searchRef} value={search} onChange={(e) => setSearch(e.target.value)} placeholder='Full-text search logs' />
              </div>
              <div className="search-wrap">
                <label>Filters</label>
                <input ref={filtersRef} value={filtersInput} onChange={(e) => setFiltersInput(e.target.value)} placeholder='field=value field!=value field~=value field:* field=low..high' />
              </div>
            </div>
            <div className="chips">
              {parsedFilters.map((clause, idx) => (
                <span key={`${clause.type}-${clause.field}-${idx}`} className="chip">
                  {formatClause(clause)}
                </span>
              ))}
            </div>
            <div className="toggles">
              {(['timestamp','level','source','context','message','payload'] as const).map((key) => (
                <label key={key}><input type="checkbox" checked={visibleCols[key]} onChange={(e) => setVisibleCols((p) => ({ ...p, [key]: e.target.checked }))} /> {key}</label>
              ))}
            </div>
            <div className="bookmarks">
              <button className="ui-btn" onClick={addBookmark}>Save bookmark</button>
              <button className="ui-btn ghost" onClick={() => setBookmarks([])} disabled={bookmarks.length === 0}>Clear</button>
              <div className="bookmark-list">
                {bookmarks.map((b, i) => (
                  <button key={`${b.search}|${b.filters}|${i}`} className="bookmark-pill" onClick={() => { setSearch(b.search); setFiltersInput(b.filters) }}>
                    {(b.search + (b.filters ? ` | ${b.filters}` : '')) || '(empty)'}
                  </button>
                ))}
              </div>
            </div>
          </section>

          <section className="panel">
            <div className="results-toolbar">
              <div>Rows <strong>{rows.length.toLocaleString()}</strong></div>
              <div>Visible <strong>{filteredRows.length.toLocaleString()}</strong></div>
              <div>Filters <strong>{parsedFilters.length}</strong></div>
              <div>Dropped <strong>{metrics?.droppedEvents ?? 0}</strong></div>
              <div>Parse errors <strong>{metrics?.parseErrors ?? malformed.length}</strong></div>
              {boot.live ? (
                <button className="ui-btn" onClick={() => setLivePaused((v) => !v)}>
                  {livePaused ? 'Resume stream' : 'Pause stream'}
                </button>
              ) : null}
              {boot.live && pendingEvents.length > 0 ? <div>Queued <strong>{pendingEvents.length}</strong></div> : null}
            </div>
            <HistogramChart buckets={histogram} />
          </section>

          <section className="panel table-panel" ref={tablePanelRef}>
            <div className="thead">
              {table.getFlatHeaders().map((header) => (
                header.isPlaceholder ? null : (
                  <div key={header.id} className="th" style={{ width: header.getSize() }}>
                    {flexRender(header.column.columnDef.header, header.getContext())}
                  </div>
                )
              ))}
            </div>
            <div className="tbody-viewport" ref={viewportRef}>
              <div style={{ height: virtualizer.getTotalSize(), position: 'relative' }}>
                {virtualRows.map((vr) => {
                  const row = rowModel[vr.index]
                  if (!row) return null
                  return (
                    <div
                      key={row.id}
                      className={`tr ${levelClass(row.original.level)} ${selected?.line === row.original.line ? 'selected' : ''}`}
                      style={{ transform: `translateY(${vr.start}px)` }}
                      onClick={() => setSelected(row.original)}
                    >
                      {row.getVisibleCells().map((cell) => (
                        <div key={cell.id} className="td" style={{ width: cell.column.getSize() }}>
                          {flexRender(cell.column.columnDef.cell, cell.getContext())}
                        </div>
                      ))}
                    </div>
                  )
                })}
              </div>
            </div>
          </section>

          <details className="panel malformed-panel" ref={malformedRef}>
            <summary>Malformed JSON lines ({malformed.length.toLocaleString()})</summary>
            <div className="malformed-list">
              {malformed.length === 0 ? (
                <div className="malformed-empty">No malformed lines.</div>
              ) : (
                malformed.slice(-200).reverse().map((m) => (
                  <div key={`${m.line}-${m.error}`} className="malformed-item">
                    <div className="malf-head"><strong>Line {m.line}</strong> <span>{m.error}</span></div>
                    <code>{m.raw}</code>
                  </div>
                ))
              )}
            </div>
          </details>
        </main>
      </div>

      <aside className={`drawer ${selected ? 'open' : ''}`}>
        <div className="drawer-head"><strong>Event details</strong><button onClick={() => setSelected(null)}>Close</button></div>
        {selected ? (
          <>
            <div className="drawer-meta">
              <div><span>Line</span><strong>{selected.line}</strong></div>
              <div><span>Timestamp</span><strong>{selected.timestamp || '-'}</strong></div>
              <div><span>Level</span><strong>{selected.level || '-'}</strong></div>
              <div><span>Source</span><strong>{selected.source || '-'}</strong></div>
              <div><span>Context</span><strong>{selected.context || '-'}</strong></div>
            </div>
            <pre className="drawer-json">{selected.prettyJSON || selected.rawJSON}</pre>
          </>
        ) : null}
      </aside>
    </div>
  )

  function addBookmark() {
    const candidate = { search: search.trim(), filters: filtersInput.trim() }
    if (!candidate.search && !candidate.filters) return
    setBookmarks((prev) => {
      const deduped = prev.filter((b) => b.search !== candidate.search || b.filters !== candidate.filters)
      return [candidate, ...deduped].slice(0, MAX_BOOKMARKS)
    })
  }
}

function LevelBadge({ level }: { level: string }) {
  const v = (level || '').toUpperCase()
  const cls = v.includes('ERROR') ? 'error' : v.includes('WARN') ? 'warn' : v.includes('DEBUG') || v.includes('TRACE') ? 'other' : 'info'
  return <span className={`level ${cls}`}>{v || '-'}</span>
}

function levelClass(level: string) {
  const v = (level || '').toLowerCase()
  if (v.includes('error') || v.includes('fatal') || v.includes('panic')) return 'lvl-error'
  if (v.includes('warn')) return 'lvl-warn'
  if (v.includes('debug') || v.includes('trace')) return 'lvl-other'
  return 'lvl-info'
}

function HistogramChart({ buckets }: { buckets: HistogramBucket[] }) {
  const ref = useRef<HTMLDivElement | null>(null)
  const chartRef = useRef<EChartsType | null>(null)
  const resizeCleanupRef = useRef<null | (() => void)>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      if (!ref.current) return
      const echarts = await import('echarts')
      if (cancelled || !ref.current) return

      const chart = (chartRef.current = chartRef.current ?? echarts.init(ref.current, undefined, { renderer: 'canvas' }))

      if (!(ref.current as any).__siftChartResizeBound) {
        const resize = () => chartRef.current?.resize()
        if ('ResizeObserver' in window) {
          const ro = new ResizeObserver(() => resize())
          ro.observe(ref.current)
          resizeCleanupRef.current = () => ro.disconnect()
        } else {
          window.addEventListener('resize', resize)
          resizeCleanupRef.current = () => window.removeEventListener('resize', resize)
        }
        ;(ref.current as any).__siftChartResizeBound = true
      }

      const labels = buckets.map((b) => b.label)
      chart.setOption(
        {
          animation: false,
          backgroundColor: 'transparent',
          grid: { left: 25, right: 25, top: 28, bottom: 26, containLabel: true },
          legend: {
            show: true,
            bottom: 0,
            itemWidth: 8,
            itemHeight: 8,
            textStyle: { color: '#9eb0cb', fontSize: 10 },
          },
          tooltip: {
            trigger: 'axis',
            axisPointer: {
              type: 'shadow',
              shadowStyle: { color: 'rgba(0, 0, 0, 0.12)' },
            },
            backgroundColor: '#1c1c25',
            borderColor: '#302f3d',
            borderWidth: 1,
            padding: 10,
            textStyle: { color: '#e6edf8', fontFamily: 'JetBrains Mono, ui-monospace, monospace', fontSize: 11 },
            formatter: (params: any) => {
              const list = Array.isArray(params) ? params : [params]
              const idx = list[0]?.dataIndex ?? 0
              const bucket = buckets[idx]
              if (!bucket) return ''
              const lines = [`${bucket.label}`, `Total: ${bucket.total}`]
              for (const p of list) if (p?.value) lines.push(`${p.marker} ${p.seriesName}: ${p.value}`)
              return lines.join('<br/>')
            },
          },
          xAxis: {
            type: 'category',
            data: labels,
            boundaryGap: true,
            axisLine: { lineStyle: { color: '#71708A' } },
            axisTick: { alignWithLabel: true },
            axisLabel: {
              color: '#9eb0cb',
              fontSize: 11,
              margin: 10,
              hideOverlap: true,
              interval: (index: number) => index % Math.max(Math.ceil(labels.length / 14), 1) !== 0,
            },
            splitLine: { show: false },
          },
          yAxis: {
            type: 'value',
            minInterval: 1,
            splitNumber: 3,
            axisLine: { show: false },
            axisTick: { show: false },
            axisPointer: { label: { precision: 0 } },
            axisLabel: {
              color: '#8fa4c3',
              fontSize: 11,
              formatter: (value: number) => {
                if (value < 1000) return String(Math.round(value))
                if (value < 1_000_000) return `${Math.round(value / 100) / 10}K`
                return `${Math.round(value / 100000) / 10}M`
              },
            },
            splitLine: { lineStyle: { color: 'rgba(120,120,140,.3)', type: 'dashed', opacity: 0.5 } },
          },
          toolbox: {
            orient: 'horizontal',
            show: true,
            showTitle: false,
            itemSize: 1,
            right: 15,
            top: 5,
            feature: {
              dataZoom: {
                show: true,
                yAxisIndex: 'none',
                icon: { zoom: 'path://', back: 'path://' },
                iconStyle: { borderWidth: 0, opacity: 0 },
                emphasis: { iconStyle: { borderWidth: 0, opacity: 0 } },
              },
            },
          },
          dataZoom: [
            {
              type: 'inside',
              xAxisIndex: 0,
              start: 0,
              end: 100,
              zoomOnMouseWheel: false,
              moveOnMouseMove: true,
              moveOnMouseWheel: false,
            },
            {
              type: 'slider',
              show: false,
              xAxisIndex: 0,
              start: 0,
              end: 100,
              brushStyle: {
                borderWidth: 1,
                borderColor: '#6871F1',
                color: 'rgba(104, 113, 241, 0.2)',
              },
              height: 15,
              bottom: 5,
            },
            {
              type: 'select',
              xAxisIndex: 0,
              brushMode: 'single',
              brushStyle: {
                borderWidth: 1,
                borderColor: '#6871F1',
                color: 'rgba(104, 113, 241, 0.2)',
              },
              transformable: true,
              throttle: 100,
            },
          ],
          series: [
            {
              name: 'INFO',
              type: 'bar',
              stack: 'levels',
              data: buckets.map((b) => b.info),
              barMaxWidth: '80%',
              barMinWidth: 2,
              barCategoryGap: '26%',
              large: true,
              largeThreshold: 100,
              itemStyle: { color: '#5470C6', borderRadius: [2, 2, 0, 0], opacity: 0.88 },
              emphasis: { itemStyle: { color: '#6a8dee', opacity: 1, shadowBlur: 4, shadowColor: 'rgba(0,0,0,.2)' } },
            },
            {
              name: 'WARN',
              type: 'bar',
              stack: 'levels',
              data: buckets.map((b) => b.warn),
              barMaxWidth: '80%',
              barMinWidth: 2,
              barCategoryGap: '26%',
              large: true,
              largeThreshold: 100,
              itemStyle: { color: '#FAC858', borderRadius: [2, 2, 0, 0], opacity: 0.9 },
              emphasis: { itemStyle: { color: '#ffd976', opacity: 1, shadowBlur: 4, shadowColor: 'rgba(0,0,0,.2)' } },
            },
            {
              name: 'ERROR',
              type: 'bar',
              stack: 'levels',
              data: buckets.map((b) => b.error),
              barMaxWidth: '80%',
              barMinWidth: 2,
              barCategoryGap: '26%',
              large: true,
              largeThreshold: 100,
              itemStyle: { color: '#EE6666', borderRadius: [2, 2, 0, 0], opacity: 0.92 },
              emphasis: { itemStyle: { color: '#ff7f7f', opacity: 1, shadowBlur: 4, shadowColor: 'rgba(0,0,0,.2)' } },
            },
            {
              name: 'OTHER',
              type: 'bar',
              stack: 'levels',
              data: buckets.map((b) => b.other),
              barMaxWidth: '80%',
              barMinWidth: 2,
              barCategoryGap: '26%',
              large: true,
              largeThreshold: 100,
              itemStyle: { color: '#9A60B4', borderRadius: [2, 2, 0, 0], opacity: 0.88 },
              emphasis: { itemStyle: { color: '#b37ad0', opacity: 1, shadowBlur: 4, shadowColor: 'rgba(0,0,0,.2)' } },
            },
          ],
        },
        { notMerge: true, lazyUpdate: true },
      )
      requestAnimationFrame(() => chart.resize())
    })()
    return () => {
      cancelled = true
    }
  }, [buckets])

  useEffect(() => () => {
    resizeCleanupRef.current?.()
    resizeCleanupRef.current = null
    chartRef.current?.dispose()
    chartRef.current = null
  }, [])

  return <div className="histogram-chart" ref={ref} aria-label="Event distribution histogram" />
}

function buildHistogram(rows: LogRow[]): HistogramBucket[] {
  if (!rows.length) return []
  const parsedTimestamps = rows.map((row) => parseTimeMs(row.timestamp))
  const validTimes = parsedTimestamps.filter((ms): ms is number => Number.isFinite(ms))

  if (validTimes.length >= Math.max(8, Math.floor(rows.length * 0.65))) {
    let minMs = validTimes[0]!
    let maxMs = validTimes[0]!
    for (let i = 1; i < validTimes.length; i++) {
      const ms = validTimes[i]!
      if (ms < minMs) minMs = ms
      if (ms > maxMs) maxMs = ms
    }
    const span = Math.max(1, maxMs - minMs)
    const bucketWidth = chooseTimeBucketWidth(span)
    const alignedMin = Math.floor(minMs / bucketWidth) * bucketWidth
    const alignedMax = Math.ceil((maxMs + 1) / bucketWidth) * bucketWidth
    const bucketCount = Math.max(1, Math.min(120, Math.ceil((alignedMax - alignedMin) / bucketWidth)))
    const buckets = Array.from({ length: bucketCount }, (_, i) => {
      const startMs = alignedMin + i * bucketWidth
      const endMs = startMs + bucketWidth
      return { startMs, endMs, label: '', info: 0, warn: 0, error: 0, other: 0, total: 0 }
    })

    rows.forEach((row, i) => {
      const ms = parsedTimestamps[i]
      if (!Number.isFinite(ms)) return
      const idx = Math.min(bucketCount - 1, Math.max(0, Math.floor((ms - alignedMin) / bucketWidth)))
      addLevelCount(buckets[idx]!, row.level)
    })

    const formatter = timeLabelFormatter(alignedMin, alignedMax)
    return buckets
      .map((b) => ({ ...b, label: formatter(b.startMs) }))
      .filter((b) => b.total > 0)
  }

  // Fallback for logs without parseable timestamps.
  const bucketCount = Math.min(80, Math.max(24, Math.ceil(Math.sqrt(rows.length) * 1.35)))
  const buckets = Array.from({ length: bucketCount }, (_, i) => ({
    startMs: i,
    endMs: i + 1,
    label: `#${i + 1}`,
    info: 0,
    warn: 0,
    error: 0,
    other: 0,
    total: 0,
  }))
  rows.forEach((row, i) => {
    const idx = Math.min(bucketCount - 1, Math.floor((i / Math.max(1, rows.length)) * bucketCount))
    addLevelCount(buckets[idx]!, row.level)
  })
  return buckets.filter((b) => b.total > 0)
}

function chooseTimeBucketWidth(spanMs: number) {
  const targetBuckets = 60
  const target = Math.max(1, Math.ceil(spanMs / targetBuckets))
  const widths = [
    1_000,
    5_000,
    10_000,
    15_000,
    30_000,
    60_000,
    5 * 60_000,
    10 * 60_000,
    15 * 60_000,
    30 * 60_000,
    60 * 60_000,
    2 * 60 * 60_000,
    3 * 60 * 60_000,
    6 * 60 * 60_000,
    12 * 60 * 60_000,
    24 * 60 * 60_000,
  ]
  for (const width of widths) if (width >= target) return width
  return widths[widths.length - 1]!
}

function addLevelCount(bucket: HistogramBucket, level: string) {
  const v = (level || '').toLowerCase()
  if (v.includes('error') || v.includes('fatal') || v.includes('panic')) bucket.error++
  else if (v.includes('warn')) bucket.warn++
  else if (v.includes('debug') || v.includes('trace')) bucket.other++
  else bucket.info++
  bucket.total++
}

function parseTimeMs(value: string) {
  const ms = Date.parse(value || '')
  return Number.isFinite(ms) ? ms : null
}

function timeLabelFormatter(minMs: number, maxMs: number) {
  const spanMs = Math.max(1, maxMs - minMs)
  const showDate = spanMs >= 24 * 60 * 60 * 1000
  const showSeconds = spanMs <= 2 * 60 * 60 * 1000
  const formatter = new Intl.DateTimeFormat(undefined, {
    ...(showDate ? { month: 'numeric', day: 'numeric' } : {}),
    hour: '2-digit',
    minute: '2-digit',
    ...(showSeconds ? { second: '2-digit' } : {}),
    hour12: false,
  })
  return (ms: number) => formatter.format(ms)
}

function tokenizeQuery(value: string) {
  const tokens: string[] = []
  let token = ''
  let quote = ''
  for (let i = 0; i < value.length; i++) {
    const ch = value[i]!
    if (quote) {
      if (ch === '\\' && i + 1 < value.length) {
        token += value[i + 1]
        i++
        continue
      }
      if (ch === quote) {
        quote = ''
        continue
      }
      token += ch
      continue
    }
    if (ch === '"' || ch === "'") {
      quote = ch
      continue
    }
    if (/\s/.test(ch)) {
      if (token) {
        tokens.push(token)
        token = ''
      }
      continue
    }
    token += ch
  }
  if (token) tokens.push(token)
  return tokens
}

function parseQueryClauses(value: string): FilterClause[] {
  const parsed: FilterClause[] = []
  for (const token of tokenizeQuery(value || '')) {
    if (!token) continue
    if (token.length > 2 && token.endsWith(':*') && !token.startsWith('.')) {
      const field = token.slice(0, -2)
      if (field) parsed.push({ type: 'exists', field })
      continue
    }
    let op = ''
    let idx = -1
    for (const candidate of ['!=', '~=', '=']) {
      const found = token.indexOf(candidate)
      if (found > 0) {
        op = candidate
        idx = found
        break
      }
    }
    if (!op) continue
    const field = token.slice(0, idx)
    const rawValue = token.slice(idx + op.length)
    if (!field || rawValue === '') continue
    if (op === '=') {
      const rangeSep = rawValue.indexOf('..')
      if (rangeSep !== -1) {
        parsed.push({
          type: 'range',
          field,
          low: rawValue.slice(0, rangeSep),
          high: rawValue.slice(rangeSep + 2),
        })
      } else {
        parsed.push({ type: 'eq', field, value: rawValue })
      }
      continue
    }
    if (op === '!=') {
      parsed.push({ type: 'neq', field, value: rawValue })
      continue
    }
    parsed.push({ type: 'contains', field, value: rawValue })
  }
  return parsed
}

function formatClause(clause: FilterClause) {
  if (clause.type === 'exists') return `${clause.field}:*`
  if (clause.type === 'range') return `${clause.field}=${clause.low}..${clause.high}`
  if (clause.type === 'eq') return `${clause.field}=${clause.value}`
  if (clause.type === 'neq') return `${clause.field}!=${clause.value}`
  return `${clause.field}~=${clause.value}`
}

function matchesClause(row: LogRow, clause: FilterClause) {
  const found = lookupField(row, clause.field)
  if (clause.type === 'exists') return found.ok && found.value != null
  if (!found.ok) return false
  const actual = normalizeValue(found.value)
  const actualLower = actual.toLowerCase()
  if (clause.type === 'eq') return actualLower === normalizeValue(clause.value).toLowerCase()
  if (clause.type === 'neq') return actualLower !== normalizeValue(clause.value).toLowerCase()
  if (clause.type === 'contains') return actualLower.includes(normalizeValue(clause.value).toLowerCase())
  return matchesRange(found.value, clause.low, clause.high)
}

function lookupField(row: LogRow, path: string): { ok: boolean; value: unknown } {
  const segs = String(path).split('.').filter(Boolean)
  if (!segs.length) return { ok: false, value: undefined }
  if (segs.length === 1) {
    const key = segs[0]!.toLowerCase()
    if (key === 'line') return { ok: true, value: row.line }
    if (key === 'timestamp') return { ok: true, value: row.timestamp }
    if (key === 'level') return { ok: true, value: row.level }
    if (key === 'source') return { ok: true, value: row.source ?? '' }
    if (key === 'context') return { ok: true, value: row.context ?? '' }
    if (key === 'message') return { ok: true, value: row.message }
  }
  let cur: unknown = getParsedPayload(row)
  for (const seg of segs) {
    if (Array.isArray(cur)) {
      if (!/^\d+$/.test(seg)) return { ok: false, value: undefined }
      const idx = Number(seg)
      cur = cur[idx]
      continue
    }
    if (!cur || typeof cur !== 'object') return { ok: false, value: undefined }
    const obj = cur as Record<string, unknown>
    if (seg in obj) {
      cur = obj[seg]
      continue
    }
    const lowerSeg = seg.toLowerCase()
    const hit = Object.keys(obj).find((k) => k.toLowerCase() === lowerSeg)
    if (!hit) return { ok: false, value: undefined }
    cur = obj[hit]
  }
  return { ok: true, value: cur }
}

function getParsedPayload(row: LogRow) {
  if (row._parsedPayload !== undefined) return row._parsedPayload
  try {
    row._parsedPayload = JSON.parse(row.rawJSON || '{}')
  } catch {
    row._parsedPayload = {}
  }
  return row._parsedPayload
}

function normalizeValue(value: unknown) {
  if (value == null) return ''
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return JSON.stringify(value)
  } catch {
    return String(value)
  }
}

function parseNumberLike(value: unknown) {
  const n = Number(String(value).trim())
  return Number.isFinite(n) ? n : null
}

function matchesRange(value: unknown, low: string, high: string) {
  const actual = normalizeValue(value)
  const numActual = parseNumberLike(actual)
  const numLow = low === '' ? null : parseNumberLike(low)
  const numHigh = high === '' ? null : parseNumberLike(high)
  if (numActual !== null && (low === '' || numLow !== null) && (high === '' || numHigh !== null)) {
    if (numLow !== null && numActual < numLow) return false
    if (numHigh !== null && numActual > numHigh) return false
    return true
  }
  const a = actual.toLowerCase()
  const l = (low || '').toLowerCase()
  const h = (high || '').toLowerCase()
  if (l && a < l) return false
  if (h && a > h) return false
  return true
}

function applyStreamEvent(
  payload: StreamEvent,
  boot: BootstrapPayload,
  setRows: Dispatch<SetStateAction<LogRow[]>>,
  setMalformed: Dispatch<SetStateAction<MalformedLine[]>>,
  setMetrics: Dispatch<SetStateAction<IngestMetrics | null>>,
) {
  if (payload.metrics) setMetrics(payload.metrics)
  if (payload.type === 'append' && payload.row) {
    setRows((prev) => {
      const next = [...prev, payload.row!]
      const cap = boot.retentionCap || 500000
      return next.length > cap ? next.slice(next.length - cap) : next
    })
    return
  }
  if (payload.type === 'malformed' && payload.malformed) {
    setMalformed((prev) => {
      const next = [...prev, payload.malformed!]
      return next.length > MAX_MALFORMED ? next.slice(next.length - MAX_MALFORMED) : next
    })
  }
}

function nextSelected(prev: LogRow | null, rows: LogRow[], delta: number) {
  if (rows.length === 0) return null
  if (!prev) return rows[0]!
  const idx = rows.findIndex((r) => r.line === prev.line)
  const nextIdx = idx < 0 ? 0 : (idx + delta + rows.length) % rows.length
  return rows[nextIdx]!
}
