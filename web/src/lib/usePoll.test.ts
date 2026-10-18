import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { usePoll } from './usePoll';

describe('usePoll hook', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('triggers initial load and polls with specified interval', async () => {
    const loadFn = vi.fn().mockResolvedValue(undefined);

    renderHook(() => usePoll(loadFn, 5, false));

    // Initial load
    expect(loadFn).toHaveBeenCalledTimes(1);

    // Let the initial promise resolve and schedule timer
    await act(async () => {
      await Promise.resolve();
      vi.advanceTimersByTime(5000);
    });

    expect(loadFn).toHaveBeenCalledTimes(2);

    // Fast-forward another 5s
    await act(async () => {
      await Promise.resolve();
      vi.advanceTimersByTime(5000);
    });

    expect(loadFn).toHaveBeenCalledTimes(3);
  });

  it('does not poll when paused or interval <= 0', async () => {
    const loadFn = vi.fn().mockResolvedValue(undefined);

    const { rerender } = renderHook(({ paused }) => usePoll(loadFn, 5, paused), {
      initialProps: { paused: true },
    });

    expect(loadFn).not.toHaveBeenCalled();

    await act(async () => {
      vi.advanceTimersByTime(10000);
    });
    expect(loadFn).not.toHaveBeenCalled();

    // Unpause
    rerender({ paused: false });
    expect(loadFn).toHaveBeenCalledTimes(1);
  });

  it('cancels in-flight request on unmount', async () => {
    let capturedSignal: AbortSignal | null = null;
    const loadFn = vi.fn().mockImplementation((signal: AbortSignal) => {
      capturedSignal = signal;
      return new Promise(() => {}); // never resolves
    });

    const { unmount } = renderHook(() => usePoll(loadFn, 5, false));

    expect(loadFn).toHaveBeenCalledTimes(1);
    expect(capturedSignal).not.toBeNull();
    expect(capturedSignal!.aborted).toBe(false);

    unmount();
    expect(capturedSignal!.aborted).toBe(true);
  });
});
