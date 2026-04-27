import { useEffect, useRef, useState } from 'react'
import type { RegisterJob, RegisterStep } from '../types'
import { getJob, jobEventsUrl } from '../api'

export interface JobStreamState {
  job: RegisterJob | null
  error: string | null
  connected: boolean
}

// useJobStream subscribes to /admin/register/jobs/{id}/events and exposes a
// merged Job snapshot. It handles three SSE event names emitted by the Go
// store: "snapshot" (initial), "step" (incremental), and "done" (terminal).
//
// The hook tolerates dropped connections by falling back to a polled snapshot
// once the EventSource fires onerror more than RECONNECT_GIVE_UP_AFTER_MS.
//
// Pass jobId = null to disable the subscription (e.g. modal closed).
export function useJobStream(jobId: string | null): JobStreamState {
  const [state, setState] = useState<JobStreamState>({ job: null, error: null, connected: false })
  const esRef = useRef<EventSource | null>(null)
  const pollTimerRef = useRef<number | null>(null)

  useEffect(() => {
    setState({ job: null, error: null, connected: false })
    if (!jobId) {
      return
    }

    let stopped = false
    let reconnectAttempts = 0
    let reconnectTimer: number | null = null

    const stopPolling = () => {
      if (pollTimerRef.current !== null) {
        window.clearInterval(pollTimerRef.current)
        pollTimerRef.current = null
      }
    }

    const beginPolling = () => {
      stopPolling()
      pollTimerRef.current = window.setInterval(async () => {
        if (stopped || !jobId) return
        try {
          const fresh = await getJob(jobId)
          setState((prev) => ({ ...prev, job: fresh, error: null }))
          if (fresh.state !== 'running') {
            stopPolling()
          }
        } catch (e: any) {
          setState((prev) => ({ ...prev, error: e?.message ?? 'poll failed' }))
        }
      }, 2000)
    }

    const connect = () => {
      if (stopped) return
      const es = new EventSource(jobEventsUrl(jobId))
      esRef.current = es

      es.addEventListener('snapshot', (ev: MessageEvent) => {
        try {
          const job = JSON.parse(ev.data) as RegisterJob
          setState({ job, error: null, connected: true })
          reconnectAttempts = 0
          stopPolling()
        } catch (e: any) {
          setState((prev) => ({ ...prev, error: 'parse snapshot: ' + (e?.message ?? '') }))
        }
      })

      es.addEventListener('step', (ev: MessageEvent) => {
        try {
          const wrap = JSON.parse(ev.data) as { step_idx: number; step: RegisterStep }
          const stepIdx = wrap.step_idx
          const step = wrap.step
          if (!step) return
          setState((prev) => {
            if (!prev.job) return prev
            const job = { ...prev.job, steps: [...prev.job.steps] }
            const targetIdx = stepIdx >= 0 && stepIdx < job.steps.length
              ? stepIdx
              : job.steps.findIndex((s) => s.email === step.email)
            if (targetIdx >= 0) {
              job.steps[targetIdx] = step
            }
            const ok = job.steps.filter((s) => s.status === 'ok').length
            const fail = job.steps.filter((s) => s.status === 'fail').length
            return { ...prev, job: { ...job, ok, fail, done: ok + fail } }
          })
        } catch {
          // ignore malformed step events; the next snapshot heals state.
        }
      })

      es.addEventListener('done', (ev: MessageEvent) => {
        try {
          const job = JSON.parse(ev.data) as RegisterJob
          setState((prev) => ({ ...prev, job, error: null, connected: true }))
        } catch {
          // ignore
        }
        stopPolling()
        es.close()
        esRef.current = null
      })

      es.onerror = () => {
        setState((prev) => ({ ...prev, connected: false }))
        es.close()
        esRef.current = null
        if (stopped) return
        reconnectAttempts += 1
        if (reconnectAttempts <= 3) {
          // Exponential backoff: 500ms, 1s, 2s.
          const delay = 500 * Math.pow(2, reconnectAttempts - 1)
          reconnectTimer = window.setTimeout(connect, delay)
        } else {
          // Give up on SSE and fall back to polling /admin/register/jobs/{id}.
          beginPolling()
        }
      }
    }

    connect()

    return () => {
      stopped = true
      if (reconnectTimer !== null) {
        window.clearTimeout(reconnectTimer)
      }
      if (esRef.current) {
        esRef.current.close()
        esRef.current = null
      }
      stopPolling()
    }
  }, [jobId])

  return state
}
