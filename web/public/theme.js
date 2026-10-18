/**
 * Observer CPA theme bridge.
 *
 * Runs before React render and before login to prevent white flashes / style mismatches.
 * In embedded mode, syncs CPA host's data-theme and whitelisted design tokens.
 * In standalone mode, honors system preference or standalone selection.
 */
(function () {
  'use strict';

  var THEME_TOKENS = [
    '--bg-secondary', '--bg-primary', '--bg-tertiary', '--bg-hover',
    '--text-primary', '--text-secondary', '--text-tertiary',
    '--muted-bg', '--muted-foreground', '--accent-bg',
    '--border-color', '--border-primary', '--border-hover',
    '--primary-color', '--primary-hover', '--primary-active', '--primary-contrast',
    '--success-color', '--warning-color', '--error-color', '--danger-color',
    '--radius-md', '--radius-lg', '--shadow', '--shadow-lg',
    '--header-height'
  ];

  function readParentTheme(parentWindow) {
    try {
      if (!parentWindow || (typeof window !== 'undefined' && parentWindow === window)) {
        return null;
      }
      var parentDocument = parentWindow.document;
      if (!parentDocument || !parentDocument.documentElement) {
        return null;
      }
      var theme = parentDocument.documentElement.getAttribute('data-theme');
      if (theme === 'dark' || theme === 'white') {
        return theme;
      }
      // CPA uses no attribute for its warm-grey light default
      return 'light';
    } catch (_) {
      return null;
    }
  }

  function prefersDarkScheme() {
    try {
      return !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    } catch (_) {
      return false;
    }
  }

  function resolveAppliedTheme(parentTheme, systemPrefersDark) {
    if (parentTheme === 'dark' || parentTheme === 'white' || parentTheme === 'light') {
      return parentTheme;
    }
    try {
      var standaloneChoice = sessionStorage.getItem('observer.theme');
      if (standaloneChoice === 'dark' || standaloneChoice === 'white' || standaloneChoice === 'light') {
        return standaloneChoice;
      }
    } catch (_) {
      // Ignore storage errors
    }
    return systemPrefersDark ? 'dark' : 'white';
  }

  function applyThemeToRoot(root, applied, parentWindow) {
    root.setAttribute('data-theme', applied);

    if (!parentWindow || typeof window === 'undefined' || typeof window.getComputedStyle !== 'function') {
      return;
    }

    var tokens = {};
    try {
      var hostRoot = parentWindow.document.documentElement;
      var hostStyle = window.getComputedStyle(hostRoot);
      for (var i = 0; i < THEME_TOKENS.length; i++) {
        var name = THEME_TOKENS[i];
        var val = hostStyle.getPropertyValue(name);
        if (typeof val === 'string') {
          val = val.trim();
          if (val) {
            tokens[name] = val;
          }
        }
      }
    } catch (_) {
      tokens = {};
    }

    for (var j = 0; j < THEME_TOKENS.length; j++) {
      var prop = THEME_TOKENS[j];
      if (typeof root.style.removeProperty === 'function') {
        root.style.removeProperty(prop);
      }
      if (Object.prototype.hasOwnProperty.call(tokens, prop)) {
        root.style.setProperty(prop, tokens[prop]);
      }
    }
  }

  function init() {
    if (typeof document === 'undefined' || !document.documentElement) {
      return;
    }
    var root = document.documentElement;
    var hostWindow = typeof window !== 'undefined' && window.parent !== window ? window.parent : null;
    var isEmbedded = hostWindow !== null;

    if (isEmbedded) {
      root.setAttribute('data-embedded', 'true');
    } else {
      root.removeAttribute('data-embedded');
    }

    var currentObserver = null;

    function sync() {
      var observed = readParentTheme(hostWindow);
      var applied = resolveAppliedTheme(observed, prefersDarkScheme());
      applyThemeToRoot(root, applied, observed === null ? null : hostWindow);
    }

    sync();

    if (hostWindow && typeof MutationObserver === 'function') {
      try {
        currentObserver = new MutationObserver(sync);
        currentObserver.observe(hostWindow.document.documentElement, {
          attributes: true,
          attributeFilter: ['data-theme']
        });
      } catch (_) {
        // Cross-origin: resolved fallback stays in effect
      }
    }

    if (!hostWindow) {
      try {
        var media = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)');
        if (media && typeof media.addEventListener === 'function') {
          media.addEventListener('change', sync);
        }
      } catch (_) {
        // matchMedia unsupported
      }
    }

    window.__OBSERVER_THEME_CLEANUP__ = function () {
      if (currentObserver) {
        currentObserver.disconnect();
        currentObserver = null;
      }
    };
  }

  init();
})();
