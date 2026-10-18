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

  /**
   * 意图：验证 zero disables polling, not initial loading。
   * 当 seconds 为 0 时，仍然必须执行一次初始加载，但后续不再自动轮询。
   */
  it('loaddep + 0 initial: executes exactly one initial load when seconds is 0 without subsequent polling', async () => {
    const loadFn = vi.fn().mockResolvedValue(undefined);

    renderHook(() => usePoll(loadFn, 0, false));

    // 初始必须被调用 1 次
    expect(loadFn).toHaveBeenCalledTimes(1);

    // 无论推进多长时间，都不会发生下一次轮询
    await act(async () => {
      await Promise.resolve();
      vi.advanceTimersByTime(20000);
    });

    expect(loadFn).toHaveBeenCalledTimes(1);
  });

  /**
   * 意图：验证 nonoverlap deferred 机制。
   * 当上一次请求耗时较长时，下一次轮询绝不重叠并发发起；
   * 必须等待当前请求 resolve 之后，才启动 seconds * 1000 延迟计时器。
   */
  it('nonoverlap deferred: never triggers overlapping loads and defers next poll until current load completes', async () => {
    let resolveLoad: () => void;
    let pending = false;

    const loadFn = vi.fn().mockImplementation(() => {
      pending = true;
      return new Promise<void>((resolve) => {
        resolveLoad = () => {
          pending = false;
          resolve();
        };
      });
    });

    renderHook(() => usePoll(loadFn, 5, false));

    // 初始第 1 次调用
    expect(loadFn).toHaveBeenCalledTimes(1);
    expect(pending).toBe(true);

    // 推进 5 秒：此时第 1 次请求依然挂起未完成，绝不能发起第 2 次重叠调用
    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(loadFn).toHaveBeenCalledTimes(1);

    // 完成第 1 次请求，此时开始 5 秒倒计时
    await act(async () => {
      resolveLoad!();
      await Promise.resolve();
    });

    // 推进 4 秒：还不到 5 秒，不应触发第 2 次
    await act(async () => {
      vi.advanceTimersByTime(4000);
    });
    expect(loadFn).toHaveBeenCalledTimes(1);

    // 再推进 1 秒（完成后的第 5 秒）：此时触发第 2 次
    await act(async () => {
      vi.advanceTimersByTime(1000);
    });
    expect(loadFn).toHaveBeenCalledTimes(2);
  });

  /**
   * 意图：验证 paused 时立即 abort 正在进行的请求，并且不再继续轮询。
   */
  it('paused abort: aborts active in-flight request when paused changes to true', async () => {
    let capturedSignal: AbortSignal | null = null;
    const loadFn = vi.fn().mockImplementation((signal: AbortSignal) => {
      capturedSignal = signal;
      return new Promise(() => {}); // 挂起
    });

    const { rerender } = renderHook(({ paused }) => usePoll(loadFn, 5, paused), {
      initialProps: { paused: false },
    });

    expect(loadFn).toHaveBeenCalledTimes(1);
    expect(capturedSignal).not.toBeNull();
    expect(capturedSignal!.aborted).toBe(false);

    // 变为 paused: true
    rerender({ paused: true });

    // 当前在途请求被立即 abort
    expect(capturedSignal!.aborted).toBe(true);

    // 推进时间，暂停态下不发生任何新调用
    await act(async () => {
      vi.advanceTimersByTime(10000);
    });
    expect(loadFn).toHaveBeenCalledTimes(1);
  });

  /**
   * 意图：验证页面隐藏时中止在途请求（hidden abort），页面重新可见时恢复轮询（resume）。
   */
  it('hidden abort + resume: aborts active request on document hidden and resumes execution on visible', async () => {
    let activeSignal: AbortSignal | null = null;
    const loadFn = vi.fn().mockImplementation((signal: AbortSignal) => {
      activeSignal = signal;
      return new Promise(() => {}); // 挂起
    });

    renderHook(() => usePoll(loadFn, 5, false));

    expect(loadFn).toHaveBeenCalledTimes(1);
    expect(activeSignal).not.toBeNull();
    expect(activeSignal!.aborted).toBe(false);

    // 模拟标签页切换到后台 (document.hidden = true)
    Object.defineProperty(document, 'hidden', { value: true, configurable: true });
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });

    // 验证在途请求被 abort
    expect(activeSignal!.aborted).toBe(true);

    // 模拟标签页重新可见 (document.hidden = false)
    Object.defineProperty(document, 'hidden', { value: false, configurable: true });
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });

    // 验证 resume 重新触发了一次执行
    expect(loadFn).toHaveBeenCalledTimes(2);
  });

  /**
   * 意图：验证当 load 回调函数变化时，旧的回调被 abort，新的回调立即触发一次 initial 加载。
   */
  it('callback changes: old aborted and one new initial triggered', async () => {
    let signal1: AbortSignal | null = null;
    const loadFn1 = vi.fn().mockImplementation((signal: AbortSignal) => {
      signal1 = signal;
      return new Promise(() => {});
    });

    let signal2: AbortSignal | null = null;
    const loadFn2 = vi.fn().mockImplementation((signal: AbortSignal) => {
      signal2 = signal;
      return new Promise(() => {});
    });

    const { rerender } = renderHook(({ fn }) => usePoll(fn, 5, false), {
      initialProps: { fn: loadFn1 },
    });

    expect(loadFn1).toHaveBeenCalledTimes(1);
    expect(signal1).not.toBeNull();
    expect(signal1!.aborted).toBe(false);

    // 依赖变更传入 loadFn2
    rerender({ fn: loadFn2 });

    // 旧在途请求被 abort
    expect(signal1!.aborted).toBe(true);

    // 新的 loadFn2 立即触发一次 initial 执行
    expect(loadFn2).toHaveBeenCalledTimes(1);
    expect(signal2).not.toBeNull();
    expect(signal2!.aborted).toBe(false);
  });

  /**
   * 意图：验证组件卸载时不仅 abort 在途请求，同时清除定时器，不再继续轮询。
   */
  it('unmount clear timer: cancels active request and clears timer so no future calls occur', async () => {
    const loadFn = vi.fn().mockImplementation(() => Promise.resolve());

    const { unmount } = renderHook(() => usePoll(loadFn, 5, false));

    expect(loadFn).toHaveBeenCalledTimes(1);

    // 在下一次定时器等待期间卸载
    unmount();

    await act(async () => {
      vi.advanceTimersByTime(20000);
    });

    expect(loadFn).toHaveBeenCalledTimes(1);
  });
});
