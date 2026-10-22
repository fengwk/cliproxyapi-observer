import { Button, Field, Input, Notice, Select } from '../components/ui';
import type {
  ConditionDraft,
  DefinitionDraft,
  FactorDraft,
  MultiplierDraft,
  OverrideDraft,
  RatePatchDraft,
  RateSetDraft,
  TtlDraft,
} from './Settings.helpers';
import {
  WEEKDAYS,
  conditionSummary,
  emptyConditionDraft,
  hasAdvancedCondition,
  moveItem,
  newMultiplierDraft,
  newOverrideDraft,
  newTtlDraft,
} from './Settings.helpers';

interface PriceDefinitionEditorProps {
  label: string;
  draft: DefinitionDraft;
  disabled: boolean;
  onChange: (next: DefinitionDraft) => void;
}

const MAX_EDITOR_DEPTH = 4;

const reasoningOptions = [
  { value: 'inherit', label: '同输出（继承）' },
  { value: 'custom', label: '独立单价' },
];

const patchReasoningOptions = [
  { value: 'inherit', label: '不修改（继承）' },
  { value: 'same_output', label: '同输出' },
  { value: 'custom', label: '独立单价' },
];

const lowerOptions = [
  { value: '', label: '不限制' },
  { value: 'gt', label: '>' },
  { value: 'gte', label: '≥' },
];

const upperOptions = [
  { value: '', label: '不限制' },
  { value: 'lt', label: '<' },
  { value: 'lte', label: '≤' },
];

function TtlRows({
  label,
  rows,
  disabled,
  onChange,
}: {
  label: string;
  rows: TtlDraft[];
  disabled: boolean;
  onChange: (rows: TtlDraft[]) => void;
}) {
  return (
    <div className="settings-definition-ttls">
      {rows.map((row, index) => (
        <div className="settings-definition-ttl-row" key={row.rowId}>
          <Field label="TTL" htmlFor={`${row.rowId}-ttl`}>
            <Input
              id={`${row.rowId}-ttl`}
              value={row.seconds}
              disabled={disabled}
              placeholder="5m / 1h / 3600s"
              aria-label={`${label} 第 ${index + 1} 档 TTL`}
              onChange={(event) => {
                const next = rows.slice();
                next[index] = { ...row, seconds: event.target.value };
                onChange(next);
              }}
            />
          </Field>
          <Field label="缓存写单价" htmlFor={`${row.rowId}-price`}>
            <Input
              id={`${row.rowId}-price`}
              value={row.price}
              disabled={disabled}
              inputMode="decimal"
              placeholder="0"
              aria-label={`${label} 第 ${index + 1} 档价格`}
              onChange={(event) => {
                const next = rows.slice();
                next[index] = { ...row, price: event.target.value };
                onChange(next);
              }}
            />
          </Field>
          <Button
            type="button"
            variant="ghost"
            disabled={disabled}
            aria-label={`${label} 删除第 ${index + 1} 档 TTL`}
            onClick={() => onChange(rows.filter((_, rowIndex) => rowIndex !== index))}
            data-testid="ttl-remove"
          >
            删除
          </Button>
        </div>
      ))}
      <Button
        type="button"
        variant="secondary"
        disabled={disabled}
        onClick={() => onChange([...rows, newTtlDraft()])}
        data-testid="ttl-add"
      >
        + 添加 TTL 档位
      </Button>
    </div>
  );
}

function ConditionGroup({
  label,
  title,
  hint,
  items,
  disabled,
  depth,
  onChange,
}: {
  label: string;
  title: string;
  hint: string;
  items: ConditionDraft[];
  disabled: boolean;
  depth: number;
  onChange: (items: ConditionDraft[]) => void;
}) {
  const canAdd = depth < MAX_EDITOR_DEPTH;
  return (
    <div className="settings-condition-group">
      <div className="settings-condition-group-header">
        <div>
          <strong>{title}</strong>
          <span className="settings-note">{hint}</span>
        </div>
        <Button
          type="button"
          variant="secondary"
          disabled={disabled || !canAdd}
          onClick={() => onChange([...items, emptyConditionDraft()])}
          data-testid={`condition-${title}-add`}
        >
          + 添加
        </Button>
      </div>
      {!canAdd ? (
        <p className="settings-note">已达到界面可编辑的嵌套深度上限；更深结构不会被丢弃。</p>
      ) : items.length === 0 ? (
        <p className="settings-note">暂无条件。</p>
      ) : (
        <div className="settings-condition-children">
          {items.map((item, index) => (
            <div className="settings-condition-child" key={`${title}-${index}`}>
              <div className="settings-condition-child-header">
                <span>条件 {index + 1}</span>
                <Button
                  type="button"
                  variant="ghost"
                  disabled={disabled}
                  aria-label={`${label} 删除${title}条件 ${index + 1}`}
                  onClick={() => onChange(items.filter((_, rowIndex) => rowIndex !== index))}
                >
                  删除
                </Button>
              </div>
              <ConditionEditor
                label={`${label} ${title}条件 ${index + 1}`}
                draft={item}
                disabled={disabled}
                depth={depth + 1}
                onChange={(next) => {
                  const values = items.slice();
                  values[index] = next;
                  onChange(values);
                }}
              />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function ConditionEditor({
  label,
  draft,
  disabled,
  onChange,
  depth = 0,
}: {
  label: string;
  draft: ConditionDraft;
  disabled: boolean;
  onChange: (next: ConditionDraft) => void;
  depth?: number;
}) {
  const patch = (next: Partial<ConditionDraft>) => onChange({ ...draft, ...next });
  const canNest = depth < MAX_EDITOR_DEPTH;
  return (
    <div className="settings-condition-editor">
      <div className="settings-definition-summary">条件：{conditionSummary(draft)}</div>
      <div className="settings-rule-conditions-grid">
        <Field label="上下文下限" hint="未缓存输入 + 缓存读 + 所有缓存写">
          <div className="settings-condition-combo">
            <Select
              aria-label={`${label} 上下文下限比较方式`}
              value={draft.context.lowerOp}
              disabled={disabled}
              options={lowerOptions}
              onChange={(value) =>
                patch({
                  context: {
                    ...draft.context,
                    lowerOp: value as ConditionDraft['context']['lowerOp'],
                  },
                })
              }
            />
            <Input
              value={draft.context.lowerValue}
              disabled={disabled}
              inputMode="numeric"
              placeholder="例如 200000"
              aria-label={`${label} 上下文下限阈值`}
              onChange={(event) =>
                patch({ context: { ...draft.context, lowerValue: event.target.value } })
              }
            />
          </div>
        </Field>
        <Field label="上下文上限">
          <div className="settings-condition-combo">
            <Select
              aria-label={`${label} 上下文上限比较方式`}
              value={draft.context.upperOp}
              disabled={disabled}
              options={upperOptions}
              onChange={(value) =>
                patch({
                  context: {
                    ...draft.context,
                    upperOp: value as ConditionDraft['context']['upperOp'],
                  },
                })
              }
            />
            <Input
              value={draft.context.upperValue}
              disabled={disabled}
              inputMode="numeric"
              placeholder="例如 400000"
              aria-label={`${label} 上下文上限阈值`}
              onChange={(event) =>
                patch({ context: { ...draft.context, upperValue: event.target.value } })
              }
            />
          </div>
        </Field>
        <Field label="服务档位" hint="逗号分隔，精确匹配实际上游档位">
          <Input
            value={draft.serviceTier}
            disabled={disabled}
            placeholder="例如 default, flex"
            aria-label={`${label} 服务档位`}
            onChange={(event) => patch({ serviceTier: event.target.value, serviceTierPresent: true })}
          />
        </Field>
        <Field label="日期下限（UTC）">
          <Input
            type="date"
            value={draft.dateGte}
            disabled={disabled}
            aria-label={`${label} 日期下限`}
            onChange={(event) => patch({ dateGte: event.target.value })}
          />
        </Field>
        <Field label="日期上限（不含）">
          <Input
            type="date"
            value={draft.dateLt}
            disabled={disabled}
            aria-label={`${label} 日期上限`}
            onChange={(event) => patch({ dateLt: event.target.value })}
          />
        </Field>
        <Field label="每日开始">
          <Input
            value={draft.timeStart}
            disabled={disabled}
            inputMode="numeric"
            placeholder="HH:mm"
            aria-label={`${label} 时间开始`}
            onChange={(event) => patch({ timeStart: event.target.value })}
          />
        </Field>
        <Field label="每日结束" hint="左闭右开，支持跨午夜；可填 24:00 表示全天结束">
          <Input
            value={draft.timeEnd}
            disabled={disabled}
            inputMode="numeric"
            placeholder="HH:mm 或 24:00"
            aria-label={`${label} 时间结束`}
            onChange={(event) => patch({ timeEnd: event.target.value })}
          />
        </Field>
        <Field label="IANA 时区">
          <Input
            value={draft.timezone}
            disabled={disabled}
            placeholder="UTC"
            aria-label={`${label} 时区`}
            onChange={(event) => patch({ timezone: event.target.value })}
          />
        </Field>
      </div>
      <div className="settings-weekday-field">
        <span className="cpa-label">适用星期</span>
        <div className="settings-weekday-grid" role="group" aria-label={`${label} 适用星期`}>
          {WEEKDAYS.map((day) => {
            const checked = draft.weekdays.includes(day.value);
            return (
              <label key={day.value} className="settings-weekday-option">
                <input
                  type="checkbox"
                  checked={checked}
                  disabled={disabled}
                  aria-label={`${label} 星期${day.label}`}
                  onChange={() =>
                    patch({
                      weekdays: checked
                        ? draft.weekdays.filter((value) => value !== day.value)
                        : [...draft.weekdays, day.value],
                    })
                  }
                />
                <span>{day.label}</span>
              </label>
            );
          })}
        </div>
        <span className="settings-note">跨午夜时按起始日归属；留空表示每天。</span>
      </div>

      <details className="settings-condition-advanced" open={hasAdvancedCondition(draft)}>
        <summary>组合条件（all / any / not）</summary>
        {!canNest ? (
          <Notice tone="info">已达到界面可编辑的嵌套深度上限；更深结构会原位保留。</Notice>
        ) : (
          <>
            <ConditionGroup
              label={label}
              title="全部满足（all）"
              hint="组内每一项都必须满足，并与上方固定条件同时生效。"
              items={draft.all}
              disabled={disabled}
              depth={depth}
              onChange={(all) => patch({ all, allPresent: true })}
            />
            <ConditionGroup
              label={label}
              title="任一满足（any）"
              hint="组内至少一项满足，并与上方固定条件同时生效。"
              items={draft.any}
              disabled={disabled}
              depth={depth}
              onChange={(any) => patch({ any, anyPresent: true })}
            />
            <div className="settings-condition-group">
              <div className="settings-condition-group-header">
                <div>
                  <strong>非（not）</strong>
                  <span className="settings-note">对一个嵌套条件取反。</span>
                </div>
                {draft.not ? (
                  <Button
                    type="button"
                    variant="ghost"
                    disabled={disabled}
                    onClick={() => patch({ not: null })}
                    data-testid="condition-not-remove"
                  >
                    删除
                  </Button>
                ) : (
                  <Button
                    type="button"
                    variant="secondary"
                    disabled={disabled}
                    onClick={() => patch({ not: emptyConditionDraft() })}
                    data-testid="condition-not-add"
                  >
                    + 添加
                  </Button>
                )}
              </div>
              {draft.not && (
                <div className="settings-condition-child">
                  <ConditionEditor
                    label={`${label} 非条件`}
                    draft={draft.not}
                    disabled={disabled}
                    depth={depth + 1}
                    onChange={(not) => patch({ not })}
                  />
                </div>
              )}
            </div>
          </>
        )}
      </details>
    </div>
  );
}

function PatchRates({
  label,
  draft,
  disabled,
  onChange,
}: {
  label: string;
  draft: RatePatchDraft;
  disabled: boolean;
  onChange: (next: RatePatchDraft) => void;
}) {
  const patch = (next: Partial<RatePatchDraft>) => onChange({ ...draft, ...next });
  const fields: Array<{
    key: keyof Pick<RatePatchDraft, 'input' | 'output' | 'cache_read' | 'cache_write'>;
    label: string;
  }> = [
    { key: 'input', label: '输入价' },
    { key: 'output', label: '输出价' },
    { key: 'cache_read', label: '缓存读价' },
    { key: 'cache_write', label: '缓存写价' },
  ];
  return (
    <>
      <div className="settings-rates-grid">
        {fields.map((field) => (
          <Field label={`${field.label}（留空继承）`} key={field.key}>
            <Input
              value={draft[field.key]}
              disabled={disabled}
              inputMode="decimal"
              placeholder="继承"
              aria-label={`${label} ${field.label}`}
              onChange={(event) =>
                patch({ [field.key]: event.target.value } as Partial<RatePatchDraft>)
              }
            />
          </Field>
        ))}
        <Field label="思考价">
          <div className="settings-definition-reasoning">
            <Select
              aria-label={`${label} 思考计价方式`}
              value={draft.reasoningMode}
              disabled={disabled}
              options={patchReasoningOptions}
              onChange={(value) => {
                const mode = value as RatePatchDraft['reasoningMode'];
                patch({
                  reasoningMode: mode,
                  reasoning: mode === 'custom' ? draft.reasoning || '0' : '',
                });
              }}
            />
            {draft.reasoningMode === 'custom' && (
              <Input
                value={draft.reasoning}
                disabled={disabled}
                inputMode="decimal"
                aria-label={`${label} 思考价`}
                onChange={(event) => patch({ reasoning: event.target.value })}
              />
            )}
          </div>
        </Field>
      </div>
      <Field label="缓存写入 TTL 覆盖" hint="继承不修改；显式覆盖为空表示清除该规则的所有 TTL 档位。">
        <Select
          aria-label={`${label} TTL 模式`}
          value={draft.ttlMode}
          disabled={disabled}
          options={[
            { value: 'inherit', label: '继承（不修改）' },
            { value: 'override', label: '显式覆盖' },
          ]}
          onChange={(value) => patch({ ttlMode: value as RatePatchDraft['ttlMode'] })}
        />
      </Field>
      {draft.ttlMode === 'override' && (
        <details className="settings-details" open>
          <summary>缓存写入 TTL 覆盖（{draft.ttls.length}）</summary>
          <p className="settings-note">
            {draft.ttls.length ? '仅覆盖所列 TTL 档位；未列档位继承基础或通用缓存写价。' : '显式空覆盖：清除该规则的全部 TTL 档位。'}
          </p>
          <TtlRows
            label={`${label} TTL 覆盖`}
            rows={draft.ttls}
            disabled={disabled}
            onChange={(ttls) => patch({ ttls })}
          />
        </details>
      )}
    </>
  );
}

function MultiplierFactors({
  label,
  draft,
  disabled,
  onChange,
}: {
  label: string;
  draft: FactorDraft;
  disabled: boolean;
  onChange: (next: FactorDraft) => void;
}) {
  const fields: Array<{ key: keyof FactorDraft; label: string }> = [
    { key: 'input', label: '输入倍率' },
    { key: 'output', label: '输出倍率' },
    { key: 'cache_read', label: '缓存读倍率' },
    { key: 'cache_write', label: '缓存写倍率' },
    { key: 'reasoning', label: '思考倍率' },
  ];
  return (
    <div className="settings-rates-grid">
      {fields.map((field) => (
        <Field label={`${field.label}（留空 = 1）`} key={field.key}>
          <Input
            value={draft[field.key]}
            disabled={disabled}
            inputMode="decimal"
            placeholder="1"
            aria-label={`${label} ${field.label}`}
            onChange={(event) => onChange({ ...draft, [field.key]: event.target.value })}
          />
        </Field>
      ))}
    </div>
  );
}

export function PriceDefinitionEditor({ label, draft, disabled, onChange }: PriceDefinitionEditorProps) {
  const updateBase = (patch: Partial<RateSetDraft>) =>
    onChange({ ...draft, base: { ...draft.base, ...patch } });

  const updateOverride = (index: number, next: OverrideDraft) => {
    const overrides = draft.overrides.slice();
    overrides[index] = next;
    onChange({ ...draft, overrides });
  };

  const updateMultiplier = (index: number, next: MultiplierDraft) => {
    const multipliers = draft.multipliers.slice();
    multipliers[index] = next;
    onChange({ ...draft, multipliers });
  };

  return (
    <div className="settings-definition">
      <div className="settings-definition-header">
        <strong>价格定义</strong>
        <span>USD / 百万 token；整数 micro-USD 精确保存</span>
      </div>

      <div className="settings-rates-grid">
        <Field label="输入价">
          <Input
            value={draft.base.input}
            disabled={disabled}
            inputMode="decimal"
            aria-label={`${label} 输入价格`}
            onChange={(event) => updateBase({ input: event.target.value })}
          />
        </Field>
        <Field label="输出价">
          <Input
            value={draft.base.output}
            disabled={disabled}
            inputMode="decimal"
            aria-label={`${label} 输出价格`}
            onChange={(event) => updateBase({ output: event.target.value })}
          />
        </Field>
        <Field label="缓存读价">
          <Input
            value={draft.base.cache_read}
            disabled={disabled}
            inputMode="decimal"
            aria-label={`${label} 缓存读价格`}
            onChange={(event) => updateBase({ cache_read: event.target.value })}
          />
        </Field>
        <Field label="缓存写价">
          <Input
            value={draft.base.cache_write}
            disabled={disabled}
            inputMode="decimal"
            aria-label={`${label} 缓存写价格`}
            onChange={(event) => updateBase({ cache_write: event.target.value })}
          />
        </Field>
        <Field label="思考价">
          <div className="settings-definition-reasoning">
            <Select
              aria-label={`${label} 思考计价方式`}
              value={draft.base.reasoningMode}
              disabled={disabled}
              options={reasoningOptions}
              onChange={(value) => {
                const mode = value as RateSetDraft['reasoningMode'];
                updateBase({
                  reasoningMode: mode,
                  reasoning: mode === 'custom' ? draft.base.reasoning || '0' : '',
                });
              }}
            />
            {draft.base.reasoningMode === 'custom' && (
              <Input
                value={draft.base.reasoning}
                disabled={disabled}
                inputMode="decimal"
                aria-label={`${label} 思考价格`}
                onChange={(event) => updateBase({ reasoning: event.target.value })}
              />
            )}
          </div>
        </Field>
      </div>

      <details className="settings-details">
        <summary>缓存写入 TTL 覆盖（{draft.base.ttls.length}）</summary>
        <p className="settings-note">未列档位使用通用缓存写价；TTL 用 5m、1h 或整秒。</p>
        <TtlRows
          label={`${label} TTL`}
          rows={draft.base.ttls}
          disabled={disabled}
          onChange={(ttls) => updateBase({ ttls })}
        />
      </details>

      <div className="settings-definition-rules-header">
        <div>
          <strong>条件价格覆盖</strong>
          <span className="settings-note">按顺序应用，后项覆盖同字段；留空字段继承。</span>
        </div>
        <Button
          type="button"
          variant="secondary"
          disabled={disabled}
          onClick={() =>
            onChange({
              ...draft,
              overrides: [
                ...draft.overrides,
                newOverrideDraft(draft.overrides.map((entry) => entry.id)),
              ],
            })
          }
          data-testid="definition-override-add"
        >
          + 价格覆盖
        </Button>
      </div>

      {draft.overrides.length === 0 ? (
        <p className="settings-note">暂无条件价格覆盖。</p>
      ) : (
        <div className="settings-definition-rules">
          {draft.overrides.map((override, index) => (
            <div className="settings-rule-card" key={override.rowId} data-testid="definition-override">
              <div className="settings-rule-header">
                <strong>覆盖 {index + 1}</strong>
                <div className="settings-rule-actions">
                  <Button
                    type="button"
                    variant="secondary"
                    disabled={disabled || index === 0}
                    onClick={() =>
                      onChange({ ...draft, overrides: moveItem(draft.overrides, index, 'up') })
                    }
                    aria-label={`${label} 上移价格覆盖 ${index + 1}`}
                    data-testid="override-up"
                  >
                    上移
                  </Button>
                  <Button
                    type="button"
                    variant="secondary"
                    disabled={disabled || index === draft.overrides.length - 1}
                    onClick={() =>
                      onChange({ ...draft, overrides: moveItem(draft.overrides, index, 'down') })
                    }
                    aria-label={`${label} 下移价格覆盖 ${index + 1}`}
                    data-testid="override-down"
                  >
                    下移
                  </Button>
                  <Button
                    type="button"
                    variant="danger"
                    disabled={disabled}
                    onClick={() =>
                      onChange({
                        ...draft,
                        overrides: draft.overrides.filter((_, rowIndex) => rowIndex !== index),
                      })
                    }
                    aria-label={`${label} 删除价格覆盖 ${index + 1}`}
                    data-testid="override-remove"
                  >
                    删除
                  </Button>
                </div>
              </div>
              <Field label="覆盖规则 ID">
                <Input
                  value={override.id}
                  disabled={disabled}
                  aria-label={`${label} 价格覆盖 ${index + 1} 规则 ID`}
                  onChange={(event) =>
                    updateOverride(index, { ...override, id: event.target.value })
                  }
                />
              </Field>
              <ConditionEditor
                label={`${label} 价格覆盖 ${index + 1}`}
                draft={override.condition}
                disabled={disabled}
                onChange={(condition) => updateOverride(index, { ...override, condition })}
              />
              <div className="settings-definition-subtitle">覆盖费率</div>
              <PatchRates
                label={`${label} 价格覆盖 ${index + 1}`}
                draft={override.rates}
                disabled={disabled}
                onChange={(rates) => updateOverride(index, { ...override, rates })}
              />
            </div>
          ))}
        </div>
      )}

      <div className="settings-definition-rules-header">
        <div>
          <strong>条件价格倍率</strong>
          <span className="settings-note">倍率作用于费率；留空字段倍率为 1。</span>
        </div>
        <Button
          type="button"
          variant="secondary"
          disabled={disabled}
          onClick={() =>
            onChange({
              ...draft,
              multipliers: [
                ...draft.multipliers,
                newMultiplierDraft(draft.multipliers.map((entry) => entry.id)),
              ],
            })
          }
          data-testid="definition-multiplier-add"
        >
          + 价格倍率
        </Button>
      </div>

      {draft.multipliers.length === 0 ? (
        <p className="settings-note">暂无条件价格倍率。</p>
      ) : (
        <div className="settings-definition-rules">
          {draft.multipliers.map((multiplier, index) => (
            <div
              className="settings-rule-card"
              key={multiplier.rowId}
              data-testid="definition-multiplier"
            >
              <div className="settings-rule-header">
                <strong>倍率 {index + 1}</strong>
                <div className="settings-rule-actions">
                  <Button
                    type="button"
                    variant="secondary"
                    disabled={disabled || index === 0}
                    onClick={() =>
                      onChange({
                        ...draft,
                        multipliers: moveItem(draft.multipliers, index, 'up'),
                      })
                    }
                    aria-label={`${label} 上移价格倍率 ${index + 1}`}
                    data-testid="multiplier-up"
                  >
                    上移
                  </Button>
                  <Button
                    type="button"
                    variant="secondary"
                    disabled={disabled || index === draft.multipliers.length - 1}
                    onClick={() =>
                      onChange({
                        ...draft,
                        multipliers: moveItem(draft.multipliers, index, 'down'),
                      })
                    }
                    aria-label={`${label} 下移价格倍率 ${index + 1}`}
                    data-testid="multiplier-down"
                  >
                    下移
                  </Button>
                  <Button
                    type="button"
                    variant="danger"
                    disabled={disabled}
                    onClick={() =>
                      onChange({
                        ...draft,
                        multipliers: draft.multipliers.filter(
                          (_, rowIndex) => rowIndex !== index
                        ),
                      })
                    }
                    aria-label={`${label} 删除价格倍率 ${index + 1}`}
                    data-testid="multiplier-remove"
                  >
                    删除
                  </Button>
                </div>
              </div>
              <Field label="倍率规则 ID">
                <Input
                  value={multiplier.id}
                  disabled={disabled}
                  aria-label={`${label} 价格倍率 ${index + 1} 规则 ID`}
                  onChange={(event) =>
                    updateMultiplier(index, { ...multiplier, id: event.target.value })
                  }
                />
              </Field>
              <ConditionEditor
                label={`${label} 价格倍率 ${index + 1}`}
                draft={multiplier.condition}
                disabled={disabled}
                onChange={(condition) => updateMultiplier(index, { ...multiplier, condition })}
              />
              <div className="settings-definition-subtitle">倍率因子</div>
              <MultiplierFactors
                label={`${label} 价格倍率 ${index + 1}`}
                draft={multiplier.factors}
                disabled={disabled}
                onChange={(factors) => updateMultiplier(index, { ...multiplier, factors })}
              />
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

export default PriceDefinitionEditor;
