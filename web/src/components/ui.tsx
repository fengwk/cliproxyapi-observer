import React from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import * as SelectPrimitive from '@radix-ui/react-select';

/* ------------------------------------------------------------------ Button */

export interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'danger' | 'ghost';
  loading?: boolean;
}

export const Button = React.forwardRef<HTMLButtonElement, ButtonProps>(
  ({ variant = 'secondary', loading = false, disabled, children, className = '', ...props }, ref) => {
    return (
      <button
        ref={ref}
        type={props.type || 'button'}
        disabled={disabled || loading}
        className={`cpa-btn cpa-btn-${variant} ${loading ? 'is-loading' : ''} ${className}`}
        {...props}
      >
        {loading && <span className="cpa-spinner" aria-hidden="true" />}
        <span>{children}</span>
      </button>
    );
  }
);
Button.displayName = 'Button';

/* ------------------------------------------------------------------- Input */

export interface InputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  error?: boolean;
}

export const Input = React.forwardRef<HTMLInputElement, InputProps>(
  ({ className = '', error, ...props }, ref) => {
    return (
      <input
        ref={ref}
        className={`cpa-input ${error ? 'has-error' : ''} ${className}`}
        {...props}
      />
    );
  }
);
Input.displayName = 'Input';

/* ------------------------------------------------------------------- Field */

export interface FieldProps {
  label: string;
  hint?: React.ReactNode;
  error?: string;
  htmlFor?: string;
  children: React.ReactNode;
  className?: string;
}

export function Field({ label, hint, error, htmlFor, children, className = '' }: FieldProps) {
  const errorId = htmlFor && error ? `${htmlFor}-error` : undefined;
  const hintId = htmlFor && hint ? `${htmlFor}-hint` : undefined;

  return (
    <div className={`cpa-field ${error ? 'has-error' : ''} ${className}`}>
      <label htmlFor={htmlFor} className="cpa-label">
        {label}
      </label>
      <div className="cpa-field-control">{children}</div>
      {hint && !error && (
        <div id={hintId} className="cpa-hint">
          {hint}
        </div>
      )}
      {error && (
        <div id={errorId} className="cpa-error-text" role="alert">
          {error}
        </div>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ Select */

export interface SelectOption {
  value: string;
  label: string;
  disabled?: boolean;
}

export interface SelectProps {
  value: string;
  onChange: (value: string) => void;
  options: SelectOption[];
  id?: string;
  disabled?: boolean;
  'aria-label'?: string;
  className?: string;
  searchable?: boolean;
  onSearchChange?: (query: string) => void;
  searchPlaceholder?: string;
}

export function Select({
  value,
  onChange,
  options,
  id,
  disabled,
  'aria-label': ariaLabel,
  className = '',
  searchable = false,
  onSearchChange,
  searchPlaceholder = '搜索选项...',
}: SelectProps) {
  const [open, setOpen] = React.useState(false);
  const [searchQuery, setSearchQuery] = React.useState('');

  const handleOpenChange = (nextOpen: boolean) => {
    setOpen(nextOpen);
    if (!nextOpen) {
      setSearchQuery('');
      onSearchChange?.('');
    }
  };

  const handleSearchChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const next = e.target.value;
    setSearchQuery(next);
    onSearchChange?.(next);
  };

  const selectedOption = options.find((opt) => opt.value === value);
  const displayLabel = selectedOption ? selectedOption.label : value;
  // Radix reserves "" for clearing its value; our API uses it for "all".
  // Choose an internal sentinel that cannot collide with a supplied identity.
  let emptyValue = '__observer_all__';
  while (options.some(option => option.value === emptyValue)) emptyValue += '_';

  const visibleOptions = React.useMemo(() => {
    if (!searchable || onSearchChange) {
      return options;
    }
    const q = searchQuery.trim().toLowerCase();
    if (!q) return options;
    return options.filter(
      (opt) =>
        opt.label.toLowerCase().includes(q) ||
        opt.value.toLowerCase().includes(q)
    );
  }, [searchable, onSearchChange, searchQuery, options]);

  return (
    <SelectPrimitive.Root
      value={value || emptyValue}
      open={open}
      onOpenChange={handleOpenChange}
      onValueChange={(next) => onChange(next === emptyValue ? '' : next)}
      disabled={disabled}
    >
      <SelectPrimitive.Trigger
        id={id}
        className={`cpa-select-trigger ${className}`}
        aria-label={ariaLabel}
      >
        <SelectPrimitive.Value asChild>
          <span className="cpa-select-value">{displayLabel}</span>
        </SelectPrimitive.Value>
        <span className="cpa-select-icon" aria-hidden="true">
          ▾
        </span>
      </SelectPrimitive.Trigger>

      <SelectPrimitive.Portal>
        <SelectPrimitive.Content
          className="cpa-select-content"
          position="popper"
          sideOffset={4}
          onKeyDown={(e) => {
            if (e.key === 'ArrowUp') {
              const activeEl = document.activeElement;
              const container = e.currentTarget;
              const firstOption = container.querySelector<HTMLElement>('[role="option"]:not([data-disabled])');
              if (activeEl === firstOption) {
                const searchInput = container.querySelector<HTMLInputElement>('.cpa-select-search-input');
                if (searchInput) {
                  e.preventDefault();
                  searchInput.focus();
                }
              }
            }
          }}
        >
          {searchable && (
            <div className="cpa-select-search">
              <input
                type="text"
                className="cpa-input cpa-select-search-input"
                value={searchQuery}
                placeholder={searchPlaceholder}
                aria-label="搜索选项"
                onChange={handleSearchChange}
                onKeyDown={(e) => {
                  if (e.key === 'Escape') {
                    setOpen(false);
                    return;
                  }
                  if (e.key === 'ArrowDown') {
                    e.preventDefault();
                    e.stopPropagation();
                    const container = e.currentTarget.closest('.cpa-select-content');
                    const firstOption = container?.querySelector<HTMLElement>('[role="option"]:not([data-disabled])');
                    firstOption?.focus();
                    return;
                  }
                  if (e.key === 'Enter') {
                    e.preventDefault();
                    e.stopPropagation();
                    const container = e.currentTarget.closest('.cpa-select-content');
                    const firstOption = container?.querySelector<HTMLElement>('[role="option"]:not([data-disabled])');
                    firstOption?.click();
                    return;
                  }
                  // Prevent Radix typeahead from interfering while typing in the input
                  e.stopPropagation();
                }}
                autoFocus
              />
            </div>
          )}
          <SelectPrimitive.Viewport className="cpa-select-viewport">
            {visibleOptions.length === 0 ? (
              <div className="cpa-select-empty">无匹配项</div>
            ) : (
              visibleOptions.map((opt) => (
                <SelectPrimitive.Item
                  key={opt.value}
                  value={opt.value || emptyValue}
                  disabled={opt.disabled}
                  className="cpa-select-item"
                >
                  <SelectPrimitive.ItemText>{opt.label}</SelectPrimitive.ItemText>
                </SelectPrimitive.Item>
              ))
            )}
          </SelectPrimitive.Viewport>
        </SelectPrimitive.Content>
      </SelectPrimitive.Portal>
    </SelectPrimitive.Root>
  );
}

/* ------------------------------------------------------------------ Switch */

export interface SwitchProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
  id?: string;
  disabled?: boolean;
  'aria-label'?: string;
  className?: string;
}

export function Switch({
  checked,
  onChange,
  id,
  disabled = false,
  'aria-label': ariaLabel,
  className = '',
}: SwitchProps) {
  return (
    <button
      type="button"
      role="switch"
      id={id}
      aria-checked={checked}
      aria-label={ariaLabel}
      disabled={disabled}
      onClick={() => {
        if (!disabled) {
          onChange(!checked);
        }
      }}
      className={`cpa-switch ${checked ? 'is-checked' : ''} ${className}`}
    >
      <span className="cpa-switch-thumb" aria-hidden="true" />
    </button>
  );
}

/* ------------------------------------------------------------------- Modal */

export interface ModalProps {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
  footer?: React.ReactNode;
  size?: 'md' | 'lg';
}

export function Modal({ title, onClose, children, footer, size = 'md' }: ModalProps) {
  return (
    <DialogPrimitive.Root
      open
      onOpenChange={(open) => {
        if (!open) {
          onClose();
        }
      }}
    >
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="cpa-modal-overlay" />
        <DialogPrimitive.Content
          className={`cpa-modal-content cpa-modal-${size}`}
          aria-describedby={undefined}
        >
          <div className="cpa-modal-header">
            <DialogPrimitive.Title className="cpa-modal-title">
              {title}
            </DialogPrimitive.Title>
            <DialogPrimitive.Close
              className="cpa-modal-close"
              aria-label="关闭"
            >
              ✕
            </DialogPrimitive.Close>
          </div>
          <div className="cpa-modal-body">{children}</div>
          {footer && <div className="cpa-modal-footer">{footer}</div>}
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}

/* ------------------------------------------------------------------- Badge */

export interface BadgeProps {
  tone?: 'neutral' | 'success' | 'warning' | 'danger';
  children: React.ReactNode;
  className?: string;
}

export function Badge({ tone = 'neutral', children, className = '' }: BadgeProps) {
  return <span className={`cpa-badge cpa-badge-${tone} ${className}`}>{children}</span>;
}

/* ------------------------------------------------------------------ Notice */

export interface NoticeProps {
  tone?: 'info' | 'warning' | 'danger' | 'success';
  children: React.ReactNode;
  className?: string;
}

export function Notice({ tone = 'info', children, className = '' }: NoticeProps) {
  return (
    <div className={`cpa-notice cpa-notice-${tone} ${className}`} role="status">
      {children}
    </div>
  );
}

/* -------------------------------------------------------------- EmptyState */

export interface EmptyStateProps {
  title: string;
  description?: string;
  action?: React.ReactNode;
  className?: string;
}

export function EmptyState({
  title,
  description,
  action,
  className = '',
}: EmptyStateProps) {
  return (
    <div className={`cpa-empty-state ${className}`}>
      <div className="cpa-empty-title">{title}</div>
      {description && <div className="cpa-empty-desc">{description}</div>}
      {action && <div className="cpa-empty-action">{action}</div>}
    </div>
  );
}
