import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { SettingsPage } from './Settings';
import type { ApiClient } from '../lib/api';
import type { Settings, PriceSnapshot, Health, ModelInfo, PricePreview } from '../lib/types';

describe('SettingsPage', () => {
  let mockApi: ApiClient;
  let onDirtyChange: ReturnType<typeof vi.fn<(dirty: boolean) => void>>;

  const manualDefinition = {
    base: {
      input: 3000000,
      output: 15000000,
      cache_read: 300000,
      cache_write: 3750000,
      reasoning: 0,
    },
    overrides: [],
    multipliers: [],
  };

  const initialSettings: Settings = {
    capture_enabled: false,
    max_body_bytes: 1048576,
    max_body_storage_bytes: 268435456,
    raw_retention_hours: 24,
    daily_retention_days: 365,
    pricing: {
      auto: true,
      source: 'models_dev_v2',
      prices: [
        {
          model: 'claude-3-5-sonnet',
          definition: manualDefinition,
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
        definition: {
          base: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
          overrides: [],
          multipliers: [],
        },
        source: 'auto',
        pricing_supported: true,
      },
    ],
    last_sync: '2026-10-18T12:00:00Z',
    refreshing: false,
    catalog_entries: 1500,
    source: 'models_dev_v2',
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
    { id: 'custom-zero-model' },
  ];

  const initialPreview: PricePreview = {
    items: [
      {
        model: 'gpt-4o',
        provider: 'openai',
        rates: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
        definition: {
          base: { input: 2500000, output: 10000000, cache_read: 1250000, cache_write: 2500000, reasoning: 0 },
          overrides: [],
          multipliers: [],
        },
        source: 'auto',
        pricing_supported: true,
      },
      {
        model: 'custom-zero-model',
        rates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: null },
        definition: {
          base: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: null },
          overrides: [{ id: 'night', when: {}, rates: { input: 0 } }],
          multipliers: [],
        },
        source: 'zero',
        pricing_supported: true,
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

  it('omits the redundant page title and jump links, and exposes logout at the bottom', async () => {
    const onLogout = vi.fn();
    const { container } = render(
      <SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} onLogout={onLogout} />
    );

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    expect(screen.queryByRole('heading', { level: 1, name: '设置' })).toBeNull();
    expect(container.querySelector('a[href^="#settings-"]')).toBeNull();

    const logout = screen.getByTestId('settings-logout');
    expect(logout).toHaveTextContent('退出');
    fireEvent.click(logout);
    // Settings delegates storage clearing/confirmation to App.
    expect(onLogout).toHaveBeenCalledTimes(1);
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

  // Test section draft isolation: saving Pricing preserves confirmed General settings and the complete definition.
  it('enforces section draft isolation: saving pricing preserves confirmed general settings and parses exact definition rates', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);

    await waitFor(() => {
      expect(screen.getByTestId('observer-settings')).toBeDefined();
    });

    // 1. Modify General draft (toggle capture on)
    const captureSwitch = screen.getByLabelText('保存请求内容');
    fireEvent.click(captureSwitch);

    // 2. Modify Pricing draft (add a complete manual identity and exact micro-USD rate)
    const addManualBtn = screen.getByTestId('manual-price-add');
    fireEvent.click(addManualBtn);

    fireEvent.change(screen.getByLabelText('第 2 条手工价格模型 ID'), {
      target: { value: 'deepseek-chat' },
    });
    fireEvent.change(screen.getByLabelText('第 2 条手工价格 Provider'), {
      target: { value: 'openai' },
    });
    fireEvent.change(screen.getByLabelText('第 2 条手工价格 输入价格'), {
      target: { value: '0.000001' },
    });
    fireEvent.change(screen.getByLabelText('第 2 条手工价格 输出价格'), {
      target: { value: '2' },
    });

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
      const deepseekPrice = payload.pricing.prices.find((p) => p.model === 'deepseek-chat');
      expect(deepseekPrice).toBeDefined();
      expect(deepseekPrice?.provider).toBe('openai');
      expect(deepseekPrice?.definition.base).toMatchObject({
        input: 1,
        output: 2000000,
        reasoning: null,
      });
      expect(deepseekPrice?.definition.overrides).toEqual([]);
      expect(deepseekPrice?.definition.multipliers).toEqual([]);
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

  // Test preview table display, normal zero pricing, rule count, and inherited reasoning.
  it('displays available model prices preview with normal zero pricing, rule badges, and inherited reasoning', async () => {
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
            { model: 'custom-zero-model', provider: '' },
          ],
        },
      })
    );

    // gpt-4o row: rates displayed, and reasoning: 0 displayed as $0 (not falsey hidden)
    expect(screen.getByText('gpt-4o')).toBeDefined();
    expect(screen.getByText('$2.5')).toBeDefined();
    expect(screen.getByText('$10')).toBeDefined();
    expect(screen.getAllByText('$0').length).toBeGreaterThanOrEqual(1);

    // Zero price is a normal amount, not a pending/unpriced warning.
    expect(screen.getByText('custom-zero-model')).toBeDefined();
    expect(screen.getByText('零价格')).toBeDefined();
    expect(screen.queryByText(/未定价/)).toBeNull();
    expect(screen.getByText('1 条条件规则')).toBeDefined();
    expect(screen.getByText('同输出')).toBeDefined();
  });

  // The compact row must retain labelled controls and save the chosen source;
  // verbose catalog caveats stay available without taking a permanent row.
  it('keeps public pricing controls labelled and saves the selected catalog source', async () => {
    render(<SettingsPage api={mockApi} refreshInterval={0} onDirtyChange={onDirtyChange} />);
    const source = await screen.findByRole('combobox', { name: '价格目录来源' });
    expect(screen.getByLabelText('目录来源')).toBe(source);
    expect(screen.getByLabelText('自动使用公共价格')).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByLabelText('公共价格目录说明').closest('details')).not.toHaveAttribute('open');
    expect(screen.getByText('公共价格仅供参考，手工价格优先；缺失价格按 $0 计费。')).toBeInTheDocument();

    fireEvent.keyDown(source, { key: 'ArrowDown' });
    fireEvent.click(screen.getByRole('option', { name: 'models.dev v1' }));
    expect(source).toHaveTextContent('models.dev v1');
    fireEvent.click(screen.getByTestId('settings-pricing-save'));
    await screen.findByText('价格设置已保存');
    const saved = vi.mocked(mockApi.request).mock.calls.find(call => call[1]?.method === 'PUT')?.[1]?.body as Settings;
    expect(saved.pricing.source).toBe('models_dev_v1');
    expect(saved.pricing.auto).toBe(true);
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

  // Test definition override ordering: up/down controls preserve order in the saved definition.
  it('manages definition override ordering with up/down controls and preserves order in save payload', async () => {
    const multiRuleSettings: Settings = {
      ...initialSettings,
      pricing: {
        auto: true,
        source: 'models_dev_v2',
        prices: [
          {
            model: 'model-a',
            definition: {
              base: { input: 1000000, output: 2000000, cache_read: 0, cache_write: 0, reasoning: null },
              overrides: [
                { id: 'rule-1', when: {}, rates: { input: 1000000 } },
                { id: 'rule-2', when: {}, rates: { input: 3000000 } },
              ],
              multipliers: [],
            },
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

    // The second override can move up; the first cannot.
    const moveUpBtns = screen.getAllByTestId('override-up');
    expect((moveUpBtns[0] as HTMLButtonElement).disabled).toBe(true);
    expect((moveUpBtns[1] as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(moveUpBtns[1]);

    // Save Pricing
    const savePricingBtn = screen.getByTestId('settings-pricing-save');
    fireEvent.click(savePricingBtn);

    await waitFor(() => {
      const putCalls = (mockApi.request as ReturnType<typeof vi.fn>).mock.calls.filter(
        (call) => call[0] === '/settings' && call[1]?.method === 'PUT'
      );
      expect(putCalls.length).toBe(1);
      const savedOverrides = (putCalls[0][1].body as Settings).pricing.prices[0].definition
        .overrides;
      expect(savedOverrides[0].id).toBe('rule-2');
      expect(savedOverrides[1].id).toBe('rule-1');
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

  // A manual refresh can finish after editing starts; confirmed data changes
  // must not clobber a draft. Actually resolve the deferred request.
  it('does not overwrite active user draft when a deferred manual refresh completes', async () => {
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

    fireEvent.click(screen.getByRole('button', { name: '刷新状态' }));
    await waitFor(() => expect(pollCount).toBe(2));

    // User edits draft: change capacity amount from 1 to 2
    const bodyInput = screen.getByLabelText('单条请求体上限数值');
    fireEvent.change(bodyInput, { target: { value: '2' } });

    expect((bodyInput as HTMLInputElement).value).toBe('2');
    expect(onDirtyChange).toHaveBeenLastCalledWith(true);

    // Now resolve the deferred poll with new server settings that would normally change body bytes to 5242880 (5MB)
    await act(async () => {
      resolveDeferredPoll({
        ...initialSettings,
        max_body_bytes: 5242880,
      });
    });

    // Draft should NOT be overwritten; input value must still be 2
    expect((screen.getByLabelText('单条请求体上限数值') as HTMLInputElement).value).toBe('2');
  });

  // Settings never polls, even if a caller passes a nonzero interval;
  // dialog/dirty transitions must not reload the price table.
  it('does not poll or reload on modal or dirty transitions', async () => {
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
      await act(async () => { await vi.advanceTimersByTimeAsync(10); });
      expect(settingsCalls).toBe(1);

      // Open compact modal
      const compactBtn = screen.getByTestId('health-compact');
      fireEvent.click(compactBtn);
      expect(screen.getByTestId('compact-confirm')).toBeDefined();

      // Advance time by 15s while modal is open; polling should be paused
      await act(async () => { await vi.advanceTimersByTimeAsync(15000); });
      expect(settingsCalls).toBe(1);

      // Closing a modal is not a refresh event.
      const cancelBtn = screen.getByTestId('compact-cancel');
      fireEvent.click(cancelBtn);
      await act(async () => { await vi.advanceTimersByTimeAsync(10); });
      expect(settingsCalls).toBe(1);

      // Now make draft dirty
      const captureSwitch = screen.getByLabelText('保存请求内容');
      fireEvent.click(captureSwitch);

      // Advance time again by 20s; dirty draft keeps periodic polling paused
      await act(async () => { await vi.advanceTimersByTimeAsync(20000); });
      expect(settingsCalls).toBe(1);
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

  // A state refresh cannot race a write or unmount the last valid table on
  // failure. Keep the dirty draft and allow saving it after the read settles.
  it('serializes state refresh against saves and preserves exact table nodes on refresh failure', async () => {
    const original = mockApi.request;
    let failRefresh = false;
    let rejectRefresh!: (error: Error) => void;
    mockApi.request = vi.fn((path, options) => {
      if (path === '/settings' && !options?.method && failRefresh) {
        return new Promise((_, reject) => { rejectRefresh = reject; });
      }
      return original(path, options);
    }) as ApiClient['request'];
    render(<SettingsPage api={mockApi} onDirtyChange={onDirtyChange} />);
    const row = (await screen.findByText('custom-zero-model')).closest('tr');
    fireEvent.change(screen.getByLabelText('单条请求体上限数值'), { target: { value: '2' } });
    failRefresh = true;
    fireEvent.click(screen.getByRole('button', { name: '刷新状态' }));
    await waitFor(() => expect(rejectRefresh).toBeDefined());
    const save = screen.getByTestId('settings-general-save');
    expect(save).toBeDisabled();
    fireEvent.click(save);
    expect(vi.mocked(mockApi.request).mock.calls.filter(([, opts]) => opts?.method === 'PUT')).toHaveLength(0);
    await act(async () => { rejectRefresh(new Error('state offline')); });
    expect(screen.getByText(/state offline.*保留上次成功状态/)).toBeInTheDocument();
    expect(screen.getByText('custom-zero-model').closest('tr')).toBe(row);
    expect(screen.getByLabelText('单条请求体上限数值')).toHaveValue('2');
    expect(save).toBeEnabled();
    fireEvent.click(save);
    await screen.findByText('请求记录与存储限制设置已保存');
    expect(vi.mocked(mockApi.request).mock.calls.find(([, opts]) => opts?.method === 'PUT')?.[1]?.body)
      .toMatchObject({ max_body_bytes: 2 * 1048576 });
  });

  // A narrower capture mode and non-minute TTL must survive unrelated edits.
  // Invalid TTL cannot be sent; stopping capture must not become a purge.
  it('preserves headers-only mode and exact TTL, validates bounds and turns capture off explicitly', async () => {
    const original = mockApi.request;
    mockApi.request = vi.fn((path, options) => {
      if (path === '/settings' && !options?.method) return Promise.resolve({
        ...initialSettings, capture_enabled: true, capture_mode: 'headers', capture_retention_seconds: 75,
      });
      return original(path, options);
    }) as ApiClient['request'];
    render(<SettingsPage api={mockApi} onDirtyChange={onDirtyChange} />);
    const ttl = await screen.findByLabelText('内容保留时间（秒）');
    fireEvent.change(ttl, { target: { value: '59' } });
    fireEvent.click(screen.getByTestId('settings-general-save'));
    expect(await screen.findByText(/至少 60 秒/)).toBeInTheDocument();
    const puts = () => vi.mocked(mockApi.request).mock.calls.filter(call => call[1]?.method === 'PUT');
    expect(puts()).toHaveLength(0);
    fireEvent.change(ttl, { target: { value: '75' } });
    fireEvent.click(screen.getByTestId('settings-general-save'));
    await screen.findByText('请求记录与存储限制设置已保存');
    expect(puts()[0][1]?.body).toMatchObject({ capture_enabled: true, capture_mode: 'headers', capture_retention_seconds: 75 });
    fireEvent.click(screen.getByLabelText('保存请求内容'));
    fireEvent.click(screen.getByTestId('settings-general-save'));
    await waitFor(() => expect(puts()).toHaveLength(2));
    expect(puts()[1][1]?.body).toMatchObject({ capture_enabled: false, capture_mode: 'off', capture_retention_seconds: 75 });
    expect(vi.mocked(mockApi.request).mock.calls.some(call => call[0].startsWith('/captures/'))).toBe(false);
  });

  // Destructive content deletion needs its own confirmation and API payload;
  // neither cancellation nor cleanup may silently become a global purge.
  it('separates expired cleanup and confirmed purge without touching accounting', async () => {
    const original = mockApi.request;
    mockApi.request = vi.fn((path, options) => path.startsWith('/captures/')
      ? Promise.resolve({ removed_records: 2, removed_bytes: 2048 }) : original(path, options)) as ApiClient['request'];
    render(<SettingsPage api={mockApi} onDirtyChange={onDirtyChange} />);
    fireEvent.click(await screen.findByRole('button', { name: '立即清空全部内容' }));
    fireEvent.click(screen.getByRole('button', { name: '取消' }));
    expect(vi.mocked(mockApi.request).mock.calls.some(call => call[0] === '/captures/purge')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: '清理过期内容' }));
    fireEvent.click(screen.getByRole('button', { name: '确认执行' }));
    await screen.findByText(/已移除 2 条内容/);
    expect(vi.mocked(mockApi.request).mock.calls.find(call => call[0] === '/captures/cleanup')?.[1]?.body).toBeUndefined();
    fireEvent.click(screen.getByRole('button', { name: '立即清空全部内容' }));
    fireEvent.click(screen.getByRole('button', { name: '确认执行' }));
    await waitFor(() => expect(vi.mocked(mockApi.request).mock.calls.find(call => call[0] === '/captures/purge')?.[1]?.body).toEqual({ confirm: true }));
    expect(vi.mocked(mockApi.request).mock.calls.some(call => call[0].startsWith('/keys/'))).toBe(false);
  });

  // Candidates become explicit billing mappings in the pricing draft, not a
  // model-route rewrite or an immediate server write.
  it('saves a selected catalog candidate as an explicit pricing mapping', async () => {
    const original = mockApi.request;
    const zeroDefinition = {
      base: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: null },
      overrides: [],
      multipliers: [],
    };
    mockApi.request = vi.fn((path, options) => path === '/prices/preview' ? Promise.resolve({ items: [{
      model: 'channel/model', provider: 'channel', source: 'zero', reason: 'ambiguous',
      rates: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: null },
      definition: zeroDefinition,
      pricing_supported: true,
      candidates: [{ provider: 'channel', model: 'full/model', definition: {
        base: { input: 1, output: 2, cache_read: 0, cache_write: 0, reasoning: null },
        overrides: [],
        multipliers: [],
      }, pricing_supported: true }],
    }] }) : original(path, options)) as ApiClient['request'];
    render(<SettingsPage api={mockApi} onDirtyChange={onDirtyChange} />);
    fireEvent.click(await screen.findByRole('button', { name: '为 channel/model 选择价格来源' }));
    fireEvent.click(screen.getByRole('button', { name: '加入草稿' }));
    expect(vi.mocked(mockApi.request).mock.calls.some(call => call[1]?.method === 'PUT')).toBe(false);
    fireEvent.click(screen.getByTestId('settings-pricing-save'));
    await screen.findByText('价格设置已保存');
    const saved = vi.mocked(mockApi.request).mock.calls.find(call => call[1]?.method === 'PUT')?.[1]?.body as Settings;
    expect(saved.pricing.mappings).toEqual([{ model: 'channel/model', provider: 'channel', catalog_model: 'full/model' }]);
    expect(saved.capture_enabled).toBe(initialSettings.capture_enabled);
  });

  // A failed manual preview refresh must leave the previous table node in place.
  it('retains the last successful preview table on refresh failure', async () => {
    render(<SettingsPage api={mockApi} onDirtyChange={onDirtyChange} />);
    const row = await screen.findByText('gpt-4o', { selector: 'strong' });
    const original = mockApi.request;
    mockApi.request = vi.fn((path, options) => path === '/prices/preview'
      ? Promise.reject(new Error('offline preview')) : original(path, options)) as ApiClient['request'];
    fireEvent.click(screen.getByRole('button', { name: '刷新状态' }));
    await screen.findByText(/offline preview/);
    expect(row.isConnected).toBe(true);
    expect(screen.getByText('gpt-4o', { selector: 'strong' })).toBe(row);
  });
});
