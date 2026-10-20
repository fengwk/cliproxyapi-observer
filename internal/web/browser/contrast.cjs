'use strict';

// Measure rendered sRGB colors, including transparent ancestor backgrounds.
// This is a focused regression for Observer text and shared control boundaries,
// not a claim of a complete WCAG audit (e.g. images, native widget internals).
module.exports = frame => frame.evaluate(() => {
  const context = document.createElement('canvas').getContext('2d', { willReadFrequently: true });
  function rgba(color) {
    context.clearRect(0, 0, 1, 1);
    context.fillStyle = color;
    context.fillRect(0, 0, 1, 1);
    return [...context.getImageData(0, 0, 1, 1).data].map((v, i) => i === 3 ? v / 255 : v);
  }
  function over(fg, bg) {
    return fg.slice(0, 3).map((v, i) => v * fg[3] + bg[i] * (1 - fg[3])).concat(1);
  }
  function background(el) {
    if (!el) return [255, 255, 255, 1];
    const color = rgba(getComputedStyle(el).backgroundColor);
    return color[3] === 1 ? color : over(color, background(el.parentElement));
  }
  function luminance(color) {
    return color.slice(0, 3).map(v => {
      v /= 255;
      return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4;
    }).reduce((sum, v, i) => sum + v * [0.2126, 0.7152, 0.0722][i], 0);
  }
  function ratio(a, b) {
    const x = luminance(a), y = luminance(b);
    return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
  }
  function eligible(el) {
    return el instanceof HTMLElement && el.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true }) &&
      !el.closest(':disabled,[aria-disabled="true"],[aria-hidden="true"]');
  }
  const result = { texts: 0, controls: 0, minText: Infinity, minBoundary: Infinity, failures: [] };
  function failure(kind, el, actual, required) {
    result.failures.push({ kind, element: el.tagName, class: el.className, actual, required });
  }
  function text(el, color) {
    const style = getComputedStyle(el), bg = background(el);
    const actual = ratio(over(rgba(color || style.color), bg), bg);
    const large = parseFloat(style.fontSize) >= 24 ||
      (parseFloat(style.fontSize) >= 18.667 && Number(style.fontWeight) >= 700);
    const required = large ? 3 : 4.5;
    result.texts++;
    result.minText = Math.min(result.minText, actual);
    if (actual < required) failure('text', el, actual, required);
  }
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let node; (node = walker.nextNode());) {
    if (node.textContent.trim() && eligible(node.parentElement)) text(node.parentElement);
  }
  for (const el of document.querySelectorAll('.cpa-input,.cpa-select-trigger,.cpa-select-search-input,.cpa-switch,.cpa-btn-secondary,.cpa-btn-primary,.cpa-btn-danger')) {
    if (!eligible(el)) continue;
    const style = getComputedStyle(el), adjacent = background(el.parentElement);
    // A filled control or its outline must identify its boundary.
    const actual = Math.max(ratio(background(el), adjacent), ratio(over(rgba(style.borderTopColor), adjacent), adjacent));
    result.controls++;
    result.minBoundary = Math.min(result.minBoundary, actual);
    if (actual < 3) failure('boundary', el, actual, 3);
    if (el.matches('input,textarea')) {
      text(el);
      if (el.hasAttribute('placeholder')) text(el, getComputedStyle(el, '::placeholder').color);
    }
  }
  return result;
});
