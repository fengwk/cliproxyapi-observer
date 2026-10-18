'use strict';

const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const crypto = require('node:crypto');

const EXPECTED_MANAGEMENT_HTML_SHA256 = '57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700';
const LOCAL_FALLBACK_MANAGEMENT_HTML = '';
const OFFICIAL_DOWNLOAD_URL = 'https://github.com/router-for-me/Cli-Proxy-API-Management-Center/releases/download/v1.26.5/management.html';

/**
 * 计算 Buffer 或字符串的 SHA256 十六进制摘要。
 */
function computeSHA256(data) {
  return crypto.createHash('sha256').update(data).digest('hex');
}

/**
 * 校验文件路径的 SHA256 是否与预期匹配。
 */
function verifyFileSHA256(filePath, expectedSha = EXPECTED_MANAGEMENT_HTML_SHA256) {
  const content = fs.readFileSync(filePath);
  const actualSha = computeSHA256(content);
  return {
    ok: actualSha.toLowerCase() === expectedSha.toLowerCase(),
    actual: actualSha.toLowerCase(),
    expected: expectedSha.toLowerCase(),
    content
  };
}

/**
 * 获取并核验官方 CPA Management Center v1.26.5 shell HTML。
 *
 * 优先级：
 * 1. options.htmlPath 或 process.env.CPA_MANAGEMENT_HTML 指定的路径（用于离线或指定测试）；
 * 2. 宿主已知本地路径 LOCAL_FALLBACK_MANAGEMENT_HTML；
 * 3. 官方 GitHub release 下载。
 *
 * 无论哪种来源，均强制核验 SHA256（不匹配立即抛错），并保存在独立临时目录（不污染 worktree）。
 */
async function ensureManagementHTML(options = {}) {
  const expectedSha = (options.expectedSha || EXPECTED_MANAGEMENT_HTML_SHA256).toLowerCase();
  const overridePath = options.htmlPath || process.env.CPA_MANAGEMENT_HTML;

  if (overridePath) {
    if (!fs.existsSync(overridePath)) {
      throw new Error(`CPA management HTML override file not found: ${overridePath}`);
    }
    const check = verifyFileSHA256(overridePath, expectedSha);
    if (!check.ok) {
      throw new Error(
        `CPA management HTML SHA256 mismatch for ${overridePath}: expected ${expectedSha}, got ${check.actual}`
      );
    }
    return {
      path: overridePath,
      content: check.content,
      sha256: check.actual,
      tempDir: null
    };
  }

  // 检查本地已知官方缓存路径
  if (fs.existsSync(LOCAL_FALLBACK_MANAGEMENT_HTML)) {
    const check = verifyFileSHA256(LOCAL_FALLBACK_MANAGEMENT_HTML, expectedSha);
    if (check.ok) {
      const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cpa-shell-'));
      const targetPath = path.join(tempDir, 'management.html');
      fs.writeFileSync(targetPath, check.content);
      return {
        path: targetPath,
        content: check.content,
        sha256: check.actual,
        tempDir
      };
    }
  }

  // 尝试网络下载至独立临时目录
  const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'cpa-shell-download-'));
  const targetPath = path.join(tempDir, 'management.html');
  try {
    const res = await fetch(OFFICIAL_DOWNLOAD_URL);
    if (!res.ok) {
      throw new Error(`Failed to download CPA management HTML from ${OFFICIAL_DOWNLOAD_URL}: HTTP ${res.status}`);
    }
    const buffer = Buffer.from(await res.arrayBuffer());
    const actualSha = computeSHA256(buffer);
    if (actualSha !== expectedSha) {
      throw new Error(
        `Downloaded CPA management HTML SHA256 mismatch: expected ${expectedSha}, got ${actualSha}`
      );
    }
    fs.writeFileSync(targetPath, buffer);
    return {
      path: targetPath,
      content: buffer,
      sha256: actualSha,
      tempDir
    };
  } catch (err) {
    try {
      fs.rmSync(tempDir, { recursive: true, force: true });
    } catch (_) {}
    throw err;
  }
}

module.exports = {
  EXPECTED_MANAGEMENT_HTML_SHA256,
  LOCAL_FALLBACK_MANAGEMENT_HTML,
  OFFICIAL_DOWNLOAD_URL,
  computeSHA256,
  verifyFileSHA256,
  ensureManagementHTML
};
