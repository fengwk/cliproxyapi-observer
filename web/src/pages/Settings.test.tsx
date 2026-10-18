import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { SettingsPage } from './Settings';
import type { ApiClient } from '../lib/api';
import type { Settings, PriceSnapshot, Health, ModelInfo, PricePreview } from '../lib/types';

describe('SettingsPage', () => {
  let mockApi: ApiClient;
  let onDirtyChange: ReturnType<typeof vi.fn>;

  const initialSettings: Settings = {
    capture_enabled: false,
    max_body_bytes: 1048576,
    max_body_storage_bytes: 268435456,
    raw_retention_hours: 24,
    daily_retention_days: 365,
    pricing: {
      auto: true,
      prices: [
        {
          model: 'claude-3-5-sonnet',
          rates: { input: 3000000, output: 15000000, cache_read: 300000, cache_write: 3750000, reasoning: 0 },
          source: 'manual',
        },
      ],
      rules: [
        {
          id: 'rule-night',
          model: 'claude-3-5-sonnet',
          time_range: '00:00-08:00',
          rates: { input: 1500000, output: 7500000, cache_read: 150000, cache_write: 1875000, reasoning: null },
        },
      ],
    },
  };

  const initialSnapshot: PriceSnapshot = {
    prices: [
      {
        model: 'gpt-4o',
        provider: 'openai',
        rates: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
        source: 'auto',
      },
    ],
    last_sync: '2026-10-18T12:00:00Z',
    refreshing: false,
    catalog_entries: 1500,
  };

  const initialHealth: Health = {
    database_bytes: 15728640, // 15 MB
    dropped_captures: 0,
    pending_usage: 0,
    active_requests: 2,
    last_error: '',
  };

  const initialModels: ModelInfo[] = [
    { id: 'gpt-4o', owned_by: 'openai' },
    { id: 'custom-unpriced-model' },
  ];

  const initialPreview: PricePreview = {
    items: [
      {
        model: 'gpt-4o',
        provider: 'openai',
        rates: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
        source: 'auto',
        has_rules: false,
      },
      {
        model: 'custom-unpriced-model',
        rates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: undefined },
        source: 'unpriced',
        has_rules: true,
      },
    ],
  };

  beforeEach(() => {
    onDirtyChange = vi.fn();
    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') {
          return options.body as Settings;
        }
        return initialSettings;
      }
      if (path === '/prices') {
        return initialSnapshot;
      }
      if (path === '/prices/refresh') {
        return { snapshot: { ...initialSnapshot, catalog_entries: 1520 } };
      }
      if (path === '/prices/preview') {
        return initialPreview;
      }
      if (path === '/health') {
        return initialHealth;
      }
      if (path === '/compact') {
        return { health: { ...initialHealth, database_bytes: 10485760 } };
      }
      throw new Error(`Unhandled mock path: ${path}`);
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };
  });

  afterEach(() => {
    vi.clearAllMocks();
  });

  // Test that all 3 task sections render properly on initial load
  it('renders all three task sections: 请求记录, 模型价格, 存储维护', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    // Shows loading notice initially
    expect(screen.getByTestId('settings-loading')).toBeDefined();

    // After resolution, loads main container
    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    expect(screen.getByRole('heading', { name: '请求记录' })).toBeDefined();
    expect(screen.getByRole('heading', { name: '模型价格' })).toBeDefined();
    expect(screen.getByRole('heading', { name: '存储维护' })).toBeDefined();

    // Verify accounting normal state displayed
    expect(screen.getByText('计费与存储状态正常')).toBeDefined();
    expect(screen.getAllByText('15.0 MB').length).toBeGreaterThanOrEqual(1);
  });

  // Test section draft isolation: saving General does NOT save unsaved Pricing drafts
  it('enforces section draft isolation: saving general settings preserves confirmed pricing', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // 1. Modify Pricing draft (e.g. toggle auto off)
    const pricingAutoSwitch = screen.getByLabelText('自动使用 models.dev 公共价格目录');
    fireEvent.click(pricingAutoSwitch);

    // Verify dirty callback notified
    expect(onDirtyChange).toHaveBeenCalledWith(true);

    // 2. Modify General draft (e.g. toggle capture on)
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    // 3. Click Save General Settings
    const saveGeneralBtn = screen.getByTestId('settings-general-save');
    fireEvent.click(saveGeneralBtn);

    await waitFor(() => {
      // Find the PUT request made for /settings
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const payload = putCalls[0][1].body as Settings;
      expect(payload.capture_enabled).toBe(true);
      // Pricing sent to server MUST be confirmedSettings.pricing (auto: true), NOT the unsaved draft (auto: false)!
      expect(payload.pricing.auto).toBe(true);
    });
  });

  // Test section draft isolation: saving Pricing preserves confirmed General settings and preserves reasoning: 0
  it('enforces section draft isolation: saving pricing preserves confirmed general settings and preserves explicit reasoning 0', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // 1. Modify General draft (toggle capture on)
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    // 2. Modify Pricing draft (add a manual price row with explicit reasoning 0)
    const addManualBtn = screen.getByTestId('manual-price-add');
    fireEvent.click(addManualBtn);

    const newModelInput = screen.getByLabelText('第 2 条手工价格模型 ID');
    fireEvent.change(newModelInput, { target: { value: 'deepseek-chat' } });

    // Set reasoning to explicit 0
    const newReasoningInput = screen.getByLabelText('第 2 条推理价格');
    fireEvent.change(newReasoningInput, { target: { value: '0' } });

    // 3. Save Pricing Settings
    const savePricingBtn = screen.getByTestId('settings-pricing-save');
    fireEvent.click(savePricingBtn);

    await waitFor(() => {
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const payload = putCalls[0][1].body as Settings;
      // General settings sent MUST be confirmed (capture_enabled: false), NOT the unsaved draft (true)!
      expect(payload.capture_enabled).toBe(false);
      // Pricing includes the new manual price with reasoning explicitly 0
      const deepseekPrice = payload.pricing.prices.find((p) => p.model === 'deepseek-chat');
      expect(deepseekPrice).toBeDefined();
      expect(deepseekPrice?.rates.reasoning).toBe(0);
    });
  });

  // Test dirty safety: cancelling drafts resets state and clears dirty indicator
  it('restores confirmed snapshot and resets dirty state when cancelling drafts', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Modify General
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    expect(screen.getByTestId('settings-general-reload')).toBeDefined();

    // Cancel General
    fireEvent.click(screen.getByTestId('settings-general-reload'));

    await waitFor(() => {
      expect(onDirtyChange).toHaveBeenLastCalledWith(false);
      expect(screen.queryByTestId('settings-general-reload')).toBeNull();
    });
  });

  // Test preview table display, unpriced warning, has_rules badge, and falsey reasoning 0 display
  it('displays available model prices preview with unpriced warning, rule badges, and explicit reasoning 0', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Check preview call was made with CPA model IDs
    expect(mockApi.request).toHaveBeenCalledWith(
      '/prices/preview',
      expect.objectContaining({
        method: 'POST',
        body: {
          models: [
            { model: 'gpt-4o', provider: '' },
            { model: 'custom-unpriced-model', provider: '' },
          ],
        },
      })
    );

    // gpt-4o row: rates displayed, and reasoning: 0 displayed as $0 (not falsey hidden)
    expect(screen.getByText('gpt-4o')).toBeDefined();
    expect(screen.getByText('$2.5')).toBeDefined();
    expect(screen.getByText('$10')).toBeDefined();
    expect(screen.getAllByText('$0').length).toBeGreaterThanOrEqual(1);

    // custom-unpriced-model row shows distinct warning and rule annotation
    expect(screen.getByText('custom-unpriced-model')).toBeDefined();
    expect(screen.getByText('未定价（按 $0 计费）')).toBeDefined();
    expect(screen.getByText('含条件规则')).toBeDefined();
  });

  // Test catalog refresh failure displays honest error and retains cache
  it('honestly displays catalog refresh failure without false success', async () => {
    const mockRequest = vi.fn(async (path: string) => {
      if (path === '/settings') return initialSettings;
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      if (path === '/prices/refresh') {
        // HTTP 200 but snapshot.error is present
        return {
          snapshot: {
            ...initialSnapshot,
            error: 'models.dev upstream timeout',
          },
        };
      }
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    const refreshBtn = screen.getByTestId('prices-refresh');
    fireEvent.click(refreshBtn);

    await waitFor(() => {
      // Must NOT render "已刷新"
      expect(screen.queryByText('价格目录已刷新')).toBeNull();
      // Must display honest error
      expect(screen.getByText(/价格目录刷新失败，已保留有效缓存：models\.dev upstream timeout/)).toBeDefined();
      // Existing cache must still be shown
      expect(screen.getByText(/公共目录总收录：1,500 条/)).toBeDefined();
    });
  });

  // Test storage maintenance compact cancellation and confirmation
  it('supports compact confirmation dialog and honors cancellation without firing compact API', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    const compactBtn = screen.getByTestId('health-compact');
    fireEvent.click(compactBtn);

    // Modal dialog is displayed
    expect(screen.getByText(/确定要执行数据库空间整理吗？/)).toBeDefined();

    // Click Cancel
    const cancelBtn = screen.getByTestId('compact-cancel');
    fireEvent.click(cancelBtn);

    // Modal closes
    await waitFor(() => {
      expect(screen.queryByText(/确定要执行数据库空间整理吗？/)).toBeNull();
    });
    // /compact API was NOT called
    expect(mockApi.request).not.toHaveBeenCalledWith('/compact', expect.anything());

    // Click compact again and confirm
    fireEvent.click(compactBtn);
    const confirmBtn = screen.getByTestId('compact-confirm');
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      expect(mockApi.request).toHaveBeenCalledWith(
        '/compact',
        expect.objectContaining({ method: 'POST' })
      );
      expect(screen.getByText('数据库压缩整理完成')).toBeDefined();
      // Database size updated from 15MB to 10MB
      expect(screen.getAllByText('10.0 MB').length).toBeGreaterThanOrEqual(1);
    });
  });

  // Test rule ordering operations: up, down, remove
  it('manages rule ordering with up/down controls and preserves order in save payload', async () => {
    const multiRuleSettings: Settings = {
      ...initialSettings,
      pricing: {
        auto: true,
        prices: [],
        rules: [
          {
            id: 'rule-1',
            model: 'model-a',
            rates: { input: 1000000, output: 2000000, cache_read: 0, cache_write: 0 },
          },
          {
            id: 'rule-2',
            model: 'model-b',
            rates: { input: 3000000, output: 4000000, cache_read: 0, cache_write: 0 },
          },
        ],
      },
    };

    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') return options.body;
        return multiRuleSettings;
      }
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Expand rules details
    const rulesSummary = screen.getByText(/条件匹配规则（高级/);
    fireEvent.click(rulesSummary);

    // Rule-2 Move Up button
    const moveUpBtns = screen.getAllByTestId('rule-up');
    // For rule 1, move up is disabled; for rule 2, move up is enabled
    expect((moveUpBtns[0] as HTMLButtonElement).disabled).toBe(true);
    expect((moveUpBtns[1] as HTMLButtonElement).disabled).toBe(false);

    // Move rule 2 up
    fireEvent.click(moveUpBtns[1]);

    // Save Pricing
    const savePricingBtn = screen.getByTestId('settings-pricing-save');
    fireEvent.click(savePricingBtn);

    await waitFor(() => {
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const savedRules = (putCalls[0][1].body as Settings).pricing.rules;
      expect(savedRules[0].id).toBe('rule-2');
      expect(savedRules[1].id).toBe('rule-1');
    });
  });

  // Test load failure handling
  it('renders honest error notice and retry button when initial load fails', async () => {
    const mockRequest = vi.fn(async () => {
      throw new Error('Connection refused');
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('settings-error')).toBeDefined();
      expect(screen.getByText('Connection refused')).toBeDefined();
    });
  });

  // Regression: exact byte capacity preservation without lossy Math.round
  it('preserves exact odd byte capacities (e.g. 1048577 bytes, 1 GiB) when editing unrelated settings', async () => {
    const oddByteSettings: Settings = {
      ...initialSettings,
      max_body_bytes: 1048577, // 1 MiB + 1 Byte
      max_body_storage_bytes: 1073741824, // Exact 1 GiB
    };

    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') return options.body;
        return oddByteSettings;
      }
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Toggle capture_enabled without touching capacity inputs
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    // Click save general
    const saveBtn = screen.getByTestId('settings-general-save');
    fireEvent.click(saveBtn);

    await waitFor(() => {
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const savedPayload = putCalls[0][1].body as Settings;
      expect(savedPayload.max_body_bytes).toBe(1048577);
      expect(savedPayload.max_body_storage_bytes).toBe(1073741824);
      expect(savedPayload.capture_enabled).toBe(true);
    });
  });

  // Regression: preview provider contract strictly decouples from model owned_by
  it('sends empty string for preview provider and does not infer provider from owned_by', async () => {
    const modelsWithOwnedBy: ModelInfo[] = [
      { id: 'meta-llama/llama-3-70b', owned_by: 'meta-llama' },
      { id: 'mistralai/mixtral-8x7b', owned_by: 'mistralai' },
    ];

    const mockRequest = vi.fn(async (path: string) => {
      if (path === '/settings') return initialSettings;
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return { items: [] };
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => modelsWithOwnedBy),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    expect(mockApi.request).toHaveBeenCalledWith(
      '/prices/preview',
      expect.objectContaining({
        method: 'POST',
        body: {
          models: [
            { model: 'meta-llama/llama-3-70b', provider: '' },
            { model: 'mistralai/mixtral-8x7b', provider: '' },
          ],
        },
      })
    );
  });

  // Regression: prevents saving before successful initial GET with deferred promise
  it('blocks saving before initial GET succeeds and guards against wiping default settings on GET failure', async () => {
    let rejectSettings!: (err: Error) => void;
    const deferredSettingsPromise = new Promise<Settings>((_resolve, reject) => {
      rejectSettings = reject;
    });

    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') return options.body;
        return deferredSettingsPromise;
      }
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    // While initial GET is unresolved, settings page is loading and save buttons do not exist
    expect(screen.getByTestId('settings-loading')).toBeDefined();
    expect(screen.queryByTestId('settings-general-save')).toBeNull();

    // Reject initial GET
    rejectSettings(new Error('Network error on load'));

    // Should display error state, save buttons still do not exist, no PUT is ever triggered
    await waitFor(() => {
      expect(screen.getByTestId('settings-error')).toBeDefined();
      expect(screen.queryByTestId('settings-general-save')).toBeNull();
    });

    const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
      (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
    );
    expect(putCalls.length).toBe(0);
  });

  // Intent: 验证延迟到达的后台轮询响应不会覆盖用户已编辑的未保存草稿 (deferred poll arriving after user edit does not clobber draft)
  it('does not overwrite active user draft when deferred background poll completes', async () => {
    let resolveDeferredPoll!: (val: Settings) => void;
    let pollCount = 0;

    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') return options.body;
        pollCount++;
        if (pollCount === 1) {
          return initialSettings;
        }
        // Second fetch: return deferred promise
        return new Promise<Settings>((resolve) => {
          resolveDeferredPoll = resolve;
        });
      }
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    // Render with 1s refresh interval
    render(<SettingsPage api={mockApi} refreshInterval={1} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // User edits draft: change capacity amount from 1 to 2
    const bodyInput = screen.getByLabelText('单条请求体上限数值');
    fireEvent.change(bodyInput, { target: { value: '2' } });

    expect((bodyInput as HTMLInputElement).value).toBe('2');
    expect(onDirtyChange).toHaveBeenLastCalledWith(true);

    // Now resolve the deferred poll with new server settings that would normally change body bytes to 5242880 (5MB)
    if (resolveDeferredPoll) {
      resolveDeferredPoll({
        ...initialSettings,
        max_body_bytes: 5242880,
      });
    }

    // Give microtasks time to run
    await new Promise((r) => setTimeout(r, 50));

    // Draft should NOT be overwritten; input value must still be 2
    expect((screen.getByLabelText('单条请求体上限数值') as HTMLInputElement).value).toBe('2');
  });

  // Intent: 验证当存在脏数据或确认弹窗打开时，轮询被暂停且不再发送周期请求 (dirty drafts or open modals stop polling)
  it('pauses periodic polling when draft is dirty or when compact modal is open', async () => {
    vi.useFakeTimers();
    try {
      let settingsCalls = 0;
      const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
        if (path === '/settings') {
          settingsCalls++;
          if (options?.method === 'PUT') return options.body;
          return initialSettings;
        }
        if (path === '/prices') return initialSnapshot;
        if (path === '/health') return initialHealth;
        if (path === '/prices/preview') return initialPreview;
        return {};
      });

      mockApi = {
        request: mockRequest as unknown as ApiClient['request'],
        models: vi.fn(async () => initialModels),
      };

      render(<SettingsPage api={mockApi} refreshInterval={5} onDirtyChange={onDirtyChange} />);

      // Initial execution
      await vi.advanceTimersByTimeAsync(10);
      expect(settingsCalls).toBe(1);

      // Open compact modal
      const compactBtn = screen.getByTestId('health-compact');
      fireEvent.click(compactBtn);
      expect(screen.getByTestId('compact-confirm')).toBeDefined();

      // Advance time by 15s while modal is open; polling should be paused
      await vi.advanceTimersByTimeAsync(15000);
      expect(settingsCalls).toBe(1);

      // Close modal (unpauses, triggering one execute)
      const cancelBtn = screen.getByTestId('compact-cancel');
      fireEvent.click(cancelBtn);
      await vi.advanceTimersByTimeAsync(10);
      expect(settingsCalls).toBe(2);

      // Now make draft dirty
      const captureSwitch = screen.getByLabelText('保存请求内容');
      fireEvent.click(captureSwitch);

      // Advance time again by 20s; dirty draft keeps periodic polling paused
      await vi.advanceTimersByTimeAsync(20000);
      expect(settingsCalls).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // Intent: 验证当模型列表或预检请求失败时显式展示错误 Notice，而非误导性显示暂无可用模型 (preview error renders error notice without showing empty state)
  it('displays explicit error notice when models or preview API fails without showing empty state', async () => {
    const mockRequest = vi.fn(async (path: string) => {
      if (path === '/settings') return initialSettings;
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') {
        throw new Error('CPA catalog unavailable');
      }
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    await waitFor(() => {
      expect(screen.getByText(/无法读取可用模型价格预检：CPA catalog unavailable/)).toBeDefined();
    });

    // Crucial: Must NOT display misleading "暂无可用模型"
    expect(screen.queryByText('暂无可用模型')).toBeNull();
  });

  // Intent: 验证请求保存文案不虚假声称保存模型响应体，准确描述仅保存脱敏请求头和请求体 (capture copy accurately reflects request-only storage without claiming response capture)
  it('accurately describes saving sanitized request content and does not claim to save model responses', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // When unchecked:
    expect(
      screen.getByText('已关闭：仅记录请求时间、模型、状态码及费用等元数据，不保存提示词与请求体（推荐）。')
    ).toBeDefined();

    // Toggle on:
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    expect(
      screen.getByText('已开启：保存脱敏请求头和请求体，可能包含敏感提示词；超限内容不保存。')
    ).toBeDefined();

    // Verify neither state claims to save model responses
    expect(screen.queryByText(/模型响应/)).toBeNull();
    expect(screen.queryByText(/完整记录/)).toBeNull();
  });

  // Intent: 验证 POST /compact 直接返回 Health 对象时，页面数据库占用空间数值真实更新 (POST /compact returns direct Health and updates UI database size)
  it('updates database bytes display when POST /compact returns a direct Health object', async () => {
    const compactHealth: Health = {
      database_bytes: 5242880, // 5.0 MB
      dropped_captures: 0,
      pending_usage: 0,
      active_requests: 0,
    };

    const mockRequest = vi.fn(async (path: string) => {
      if (path === '/settings') return initialSettings;
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth; // 15 MB
      if (path === '/compact') {
        return compactHealth; // direct Health object, not { health: ... }
      }
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
      expect(screen.getAllByText('15.0 MB').length).toBeGreaterThanOrEqual(1);
    });

    const compactBtn = screen.getByTestId('health-compact');
    fireEvent.click(compactBtn);

    const confirmBtn = screen.getByTestId('compact-confirm');
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      expect(screen.getByText('数据库压缩整理完成')).toBeDefined();
      expect(screen.getAllByText('5.0 MB').length).toBeGreaterThanOrEqual(1);
    });
  });

  // Intent: 验证同步 isMutatingRef 锁能有效防止快速连击或同 tick 双重提交保存请求 (synchronous mutation ref lock prevents duplicate save submissions)
  it('prevents double save submission on rapid consecutive clicks in the same event loop tick', async () => {
    let putCallsCount = 0;
    const mockRequest = vi.fn(async (path: string, options?: { method?: string; body?: unknown }) => {
      if (path === '/settings') {
        if (options?.method === 'PUT') {
          putCallsCount++;
          // Simulate latency
          await new Promise((r) => setTimeout(r, 50));
          return options.body;
        }
        return initialSettings;
      }
      if (path === '/prices') return initialSnapshot;
      if (path === '/health') return initialHealth;
      if (path === '/prices/preview') return initialPreview;
      return {};
    });

    mockApi = {
      request: mockRequest as unknown as ApiClient['request'],
      models: vi.fn(async () => initialModels),
    };

    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // Make draft dirty
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    const saveBtn = screen.getByTestId('settings-general-save');
    // Rapid double click in the same synchronous block
    fireEvent.click(saveBtn);
    fireEvent.click(saveBtn);

    await waitFor(() => {
      expect(screen.getByText('请求记录与存储限制设置已保存')).toBeDefined();
    });

    // Only one PUT request should have been dispatched
    expect(putCallsCount).toBe(1);
  });
});
