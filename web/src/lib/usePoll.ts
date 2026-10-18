import { useEffect, useRef } from 'react';

/**
 * Hook for non-overlapping, visibility-aware polling with cancellation.
 *
 * @param load Asynchronous load callback receiving an AbortSignal.
 * @param seconds Polling interval in seconds; <= 0 disables polling.
 * @param paused When true, pauses polling (e.g. during dirty drafts or open modals).
 */
export function usePoll(
  load: (signal: AbortSignal) => Promise<void>,
  seconds: number,
  paused?: boolean
): void {
  const loadRef = useRef(load);
  loadRef.current = load;

  const secondsRef = useRef(seconds);
  secondsRef.current = seconds;

  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  useEffect(() => {
    let timerId: ReturnType<typeof setTimeout> | null = null;
    let activeController: AbortController | null = null;
    let unmounted = false;

    const clearTimer = () => {
      if (timerId !== null) {
        clearTimeout(timerId);
        timerId = null;
      }
    };

    const abortCurrent = () => {
      if (activeController) {
        activeController.abort();
        activeController = null;
      }
    };

    const scheduleNext = () => {
      clearTimer();
      if (unmounted) return;
      if (pausedRef.current || secondsRef.current <= 0) return;
      if (typeof document !== 'undefined' && document.hidden) return;

      timerId = setTimeout(() => {
        execute();
      }, secondsRef.current * 1000);
    };

    const execute = async () => {
      clearTimer();
      if (unmounted) return;
      if (pausedRef.current) return;
      if (typeof document !== 'undefined' && document.hidden) return;

      abortCurrent();
      const controller = new AbortController();
      activeController = controller;

      try {
        await loadRef.current(controller.signal);
      } catch (err: unknown) {
        // Silently tolerate aborts
        if (controller.signal.aborted) {
          return;
        }
      } finally {
        if (activeController === controller) {
          activeController = null;
        }
        if (!unmounted && !controller.signal.aborted) {
          scheduleNext();
        }
      }
    };

    // Visibility listener
    const handleVisibilityChange = () => {
      if (document.hidden) {
        clearTimer();
        abortCurrent();
      } else {
        if (!pausedRef.current && secondsRef.current > 0) {
          execute();
        }
      }
    };

    if (typeof document !== 'undefined') {
      document.addEventListener('visibilitychange', handleVisibilityChange);
    }

    // Trigger initial execution if not paused
    if (!paused) {
      execute();
    }

    return () => {
      unmounted = true;
      clearTimer();
      abortCurrent();
      if (typeof document !== 'undefined') {
        document.removeEventListener('visibilitychange', handleVisibilityChange);
      }
    };
  }, [seconds, paused]);
}
