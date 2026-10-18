import { useEffect } from 'react';

/** One initial load per dependency generation; subsequent loads never overlap. */
export function usePoll(
  load: (signal: AbortSignal) => Promise<void>, seconds: number, paused = false,
): void {
  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    let active: AbortController | undefined;
    let disposed = false;
    function cancel() {
      clearTimeout(timer);
      active?.abort();
      active = undefined;
    }
    async function execute() {
      if (disposed || paused || document.hidden || active) return;
      const controller = new AbortController();
      active = controller;
      try { await load(controller.signal); }
      catch { /* pages present non-abort errors beside their data */ }
      finally {
        if (active === controller) active = undefined;
        if (!disposed && !paused && !controller.signal.aborted && seconds > 0 && !document.hidden) {
          timer = setTimeout(execute, seconds * 1000);
        }
      }
    }
    function visibilityChanged() {
      if (document.hidden) cancel();
      else if (seconds > 0) void execute();
    }
    document.addEventListener('visibilitychange', visibilityChanged);
    void execute(); // zero disables polling, not the initial request
    return () => {
      disposed = true;
      cancel();
      document.removeEventListener('visibilitychange', visibilityChanged);
    };
  }, [load, seconds, paused]);
}
