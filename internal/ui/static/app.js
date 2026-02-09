// Auto-refresh interval handles
let cacheRefreshInterval = null;
let liveStatsInterval = null;

// Relay state
let relayActive = false;
let relayAbortController = null;

// Fetch method descriptions for the hint text
const fetchMethodHints = {
    sw_fallback: 'Service Worker intercepts addon requests in the browser; falls back to server-side fetch if SW is unavailable. Works with Cloudflare-protected addons when using Stremio Web.',
    tab_relay: 'Bridge UI tab acts as a relay for addon requests using your browser\'s IP. Requires this tab to stay open while streaming.',
    sw_only: 'All addon fetching through Service Worker only. No server-side fallback. Requires Stremio Web with SW injection configured.',
    direct: 'Server fetches from addons directly using the PCS server IP. Does NOT work with Cloudflare-protected addons (e.g., Torrentio).',
    proxy: 'Server fetches through a custom HTTP/SOCKS proxy. Useful if you have a residential proxy or VPN.',
};

// Initialize on page load
document.addEventListener('DOMContentLoaded', () => {
    loadConfig();
    loadAddons();
    loadCacheStats();
    loadLiveStats();

    // Fetch method dropdown change handler
    const fetchMethodSelect = document.getElementById('fetch-method-select');
    fetchMethodSelect.addEventListener('change', () => {
        updateFetchMethodHint();
        updateProxyVisibility();
    });

    // Auto-refresh cache stats every 30 seconds
    cacheRefreshInterval = setInterval(loadCacheStats, 30000);

    // Live torrent stats every 3 seconds
    liveStatsInterval = setInterval(loadLiveStats, 3000);

    // Check if relay should be active (after config and addons load)
    setTimeout(checkRelayNeeded, 2000);
});

// ---------------------------------------------------------------------------
// Fetch Method Helpers
// ---------------------------------------------------------------------------

function updateFetchMethodHint() {
    const method = document.getElementById('fetch-method-select').value;
    const hintEl = document.getElementById('fetch-method-hint');
    hintEl.textContent = fetchMethodHints[method] || '';
}

function updateProxyVisibility() {
    const method = document.getElementById('fetch-method-select').value;
    const proxyGroup = document.getElementById('proxy-url-group');
    proxyGroup.style.display = method === 'proxy' ? 'block' : 'none';
}

// ---------------------------------------------------------------------------
// Browser Tab Relay
// ---------------------------------------------------------------------------

// Check if any addon uses tab_relay (directly or via global default)
async function checkRelayNeeded() {
    try {
        const [configResp, addonsResp] = await Promise.all([
            fetch('/api/config'),
            fetch('/api/addons'),
        ]);
        if (!configResp.ok || !addonsResp.ok) return;

        const config = await configResp.json();
        const addons = await addonsResp.json();

        const globalMethod = config.defaultFetchMethod || 'sw_fallback';

        // Relay is needed if any addon's effective method is tab_relay,
        // OR if sw_fallback is active and any addon exists (relay used as fallback).
        let needRelay = false;
        if (addons && addons.length > 0) {
            for (const addon of addons) {
                const effective = (addon.fetchMethod === 'global' || !addon.fetchMethod)
                    ? globalMethod
                    : addon.fetchMethod;
                if (effective === 'tab_relay' || effective === 'sw_fallback') {
                    needRelay = true;
                    break;
                }
            }
        }

        if (needRelay && !relayActive) {
            startRelay();
        } else if (!needRelay && relayActive) {
            stopRelay();
        }
    } catch (e) {
        console.error('Failed to check relay status:', e);
    }
}

function startRelay() {
    if (relayActive) return;
    relayActive = true;

    const section = document.getElementById('relay-section');
    section.style.display = 'block';
    updateRelayUI(true);

    relayPollLoop();
}

function stopRelay() {
    relayActive = false;
    if (relayAbortController) {
        relayAbortController.abort();
        relayAbortController = null;
    }

    const section = document.getElementById('relay-section');
    section.style.display = 'none';
}

function updateRelayUI(connected) {
    const section = document.getElementById('relay-section');
    const label = document.getElementById('relay-label');

    if (connected) {
        section.classList.remove('relay-disconnected');
        label.textContent = 'Relay Active';
    } else {
        section.classList.add('relay-disconnected');
        label.textContent = 'Relay Disconnected';
    }
}

async function relayPollLoop() {
    while (relayActive) {
        try {
            relayAbortController = new AbortController();
            const response = await fetch('/api/relay/pending', {
                signal: relayAbortController.signal,
            });

            updateRelayUI(true);

            if (response.status === 204) {
                // No pending request, loop again immediately
                continue;
            }

            if (!response.ok) {
                throw new Error(`HTTP ${response.status}`);
            }

            const request = await response.json();
            // Process the relay request in the background
            processRelayRequest(request);
        } catch (e) {
            if (e.name === 'AbortError') {
                break; // Relay was stopped
            }
            console.error('Relay poll error:', e);
            updateRelayUI(false);
            // Wait before retrying on error
            await new Promise(r => setTimeout(r, 3000));
        }
    }
}

async function processRelayRequest(request) {
    const { id, url } = request;

    try {
        const response = await fetch(url);
        const body = await response.text();

        await fetch(`/api/relay/response/${id}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
                statusCode: response.status,
                body: body,
            }),
        });
    } catch (e) {
        console.error(`Relay fetch failed for ${url}:`, e);
        // Report the error back to the server
        try {
            await fetch(`/api/relay/response/${id}`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    statusCode: 0,
                    body: '',
                    error: e.message,
                }),
            });
        } catch (e2) {
            console.error('Failed to report relay error:', e2);
        }
    }
}

// ---------------------------------------------------------------------------
// Config & Engine Status
// ---------------------------------------------------------------------------

// Load configuration from API
async function loadConfig() {
    try {
        const response = await fetch('/api/config');
        if (!response.ok) throw new Error(`HTTP ${response.status}`);

        const config = await response.json();

        // Populate form fields
        document.getElementById('engine-select').value = config.defaultEngine || 'torrserver';
        document.getElementById('fetch-method-select').value = config.defaultFetchMethod || 'sw_fallback';
        document.getElementById('proxy-url').value = config.proxyURL || '';
        document.getElementById('cache-size').value = config.cacheSizeGB || 50;
        document.getElementById('cache-age').value = config.cacheMaxAgeDays || 30;

        // Update hints and visibility
        updateFetchMethodHint();
        updateProxyVisibility();

        // Show engine status
        const statusEl = document.getElementById('engine-status');
        if (config.engines && Object.keys(config.engines).length > 0) {
            let statusHTML = '<div class="engine-grid">';
            for (const [engine, info] of Object.entries(config.engines)) {
                const isOnline = info.status === 'online';
                const isActive = engine === config.defaultEngine;
                const badgeClass = isOnline ? 'badge-online' : (info.status === 'offline' ? 'badge-offline' : 'badge-unknown');
                const badgeText = isOnline ? 'Online' : (info.status === 'offline' ? 'Offline' : 'Unknown');
                const activeClass = isActive ? 'engine-active' : '';

                statusHTML += `<div class="engine-row ${activeClass}">
                    <div class="engine-info">
                        <strong>${escapeHtml(engine)}</strong>
                        ${isActive ? '<span class="active-label">active</span>' : ''}
                    </div>
                    <div class="engine-url">${escapeHtml(info.url || 'Not configured')}</div>
                    <span class="status-badge ${badgeClass}">${badgeText}</span>
                </div>`;
            }
            statusHTML += '</div>';
            statusEl.innerHTML = statusHTML;
        } else {
            statusEl.textContent = 'No engines configured';
        }
    } catch (error) {
        console.error('Failed to load config:', error);
        document.getElementById('engine-status').innerHTML =
            `<span style="color: #e94560;">Failed to load config: ${escapeHtml(error.message)}</span>`;
    }
}

// Save configuration
async function saveConfig() {
    const saveBtn = document.getElementById('save-config-btn');
    const statusEl = document.getElementById('config-status');

    const config = {
        defaultEngine: document.getElementById('engine-select').value,
        defaultFetchMethod: document.getElementById('fetch-method-select').value,
        proxyURL: document.getElementById('proxy-url').value.trim(),
        cacheSizeGB: parseInt(document.getElementById('cache-size').value, 10),
        cacheMaxAgeDays: parseInt(document.getElementById('cache-age').value, 10),
    };

    // Validate
    if (config.cacheSizeGB < 1 || config.cacheSizeGB > 1000) {
        showStatus(statusEl, 'Cache size must be between 1 and 1000 GB', true);
        return;
    }

    if (config.cacheMaxAgeDays < 1 || config.cacheMaxAgeDays > 365) {
        showStatus(statusEl, 'Cache age must be between 1 and 365 days', true);
        return;
    }

    if (config.defaultFetchMethod === 'proxy' && !config.proxyURL) {
        showStatus(statusEl, 'Proxy URL is required when using Custom Proxy method', true);
        return;
    }

    // Disable button during request
    saveBtn.disabled = true;
    saveBtn.textContent = 'Saving...';

    try {
        const response = await fetch('/api/config', {
            method: 'PUT',
            headers: {
                'Content-Type': 'application/json',
            },
            body: JSON.stringify(config),
        });

        if (!response.ok) {
            const errorData = await response.json().catch(() => ({}));
            throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        // Success
        showStatus(statusEl, 'Settings saved!', false);

        // Reload config to refresh engine status
        loadConfig();

        // Re-check if relay is needed with new settings
        setTimeout(checkRelayNeeded, 500);
    } catch (error) {
        console.error('Failed to save config:', error);
        showStatus(statusEl, `Failed to save: ${error.message}`, true);
    } finally {
        saveBtn.disabled = false;
        saveBtn.textContent = 'Save Settings';
    }
}

// ---------------------------------------------------------------------------
// Addons
// ---------------------------------------------------------------------------

// Fetch method labels for display
const fetchMethodLabels = {
    global: 'Use Global',
    sw_fallback: 'SW + Fallback',
    tab_relay: 'Tab Relay',
    sw_only: 'SW Only',
    direct: 'PCS-IP Only',
    proxy: 'Custom Proxy',
};

// Load addons list from API
async function loadAddons() {
    const listEl = document.getElementById('addons-list');
    listEl.innerHTML = '<div style="text-align: center; color: #aaa;">Loading...</div>';

    try {
        const response = await fetch('/api/addons');
        if (!response.ok) throw new Error(`HTTP ${response.status}`);

        const addons = await response.json();

        if (!addons || addons.length === 0) {
            listEl.innerHTML = '<div class="empty-state">No addons configured. Add one above!</div>';
            return;
        }

        // Render addon list
        listEl.innerHTML = addons.map(addon => {
            const displayName = addon.name || extractAddonLabel(addon.originalUrl);
            const originalUrl = addon.originalUrl || '';
            const truncatedURL = originalUrl.length > 60
                ? originalUrl.substring(0, 57) + '...'
                : originalUrl;

            // Use wrappedUrl directly from API
            const wrappedURL = addon.wrappedUrl || '';

            // Fetch status indicator
            const statusClass = addon.fetchStatus === 'ok' ? 'fetch-ok'
                : addon.fetchStatus === 'blocked' ? 'fetch-blocked'
                : 'fetch-unknown';
            const statusLabel = addon.fetchStatus === 'ok' ? 'OK'
                : addon.fetchStatus === 'blocked' ? 'Blocked'
                : '?';

            // Build fetch method options
            const currentMethod = addon.fetchMethod || 'global';
            const methodOptions = Object.entries(fetchMethodLabels).map(([value, label]) => {
                const selected = value === currentMethod ? 'selected' : '';
                return `<option value="${value}" ${selected}>${escapeHtml(label)}</option>`;
            }).join('');

            return `
                <div class="addon-item">
                    <div class="addon-header">
                        <div class="addon-header-left">
                            <span class="fetch-status ${statusClass}" title="Fetch status: ${statusLabel}">${statusLabel}</span>
                            <div>
                                <div class="addon-name">${escapeHtml(displayName)}</div>
                                <div class="addon-url" title="${escapeHtml(originalUrl)}">${escapeHtml(truncatedURL)}</div>
                            </div>
                        </div>
                        <div class="addon-actions">
                            <select class="addon-fetch-select" onchange="updateAddonFetchMethod('${escapeHtml(addon.id)}', this.value)">
                                ${methodOptions}
                            </select>
                            <button class="small danger" onclick="removeAddon('${escapeHtml(addon.id)}')">Remove</button>
                        </div>
                    </div>
                    <div class="addon-wrapped">
                        <span class="addon-wrapped-label">Wrapped URL:</span>
                        <div class="addon-wrapped-url">${escapeHtml(wrappedURL)}</div>
                        <button class="small" onclick="copyToClipboard('${escapeHtml(wrappedURL)}')">Copy</button>
                    </div>
                </div>
            `;
        }).join('');
    } catch (error) {
        console.error('Failed to load addons:', error);
        listEl.innerHTML = `<div class="empty-state" style="color: #e94560;">Failed to load addons: ${escapeHtml(error.message)}</div>`;
    }
}

// Add new addon
async function addAddon() {
    const urlInput = document.getElementById('addon-url');
    const errorEl = document.getElementById('add-error');
    const addBtn = document.getElementById('add-btn');

    const url = urlInput.value.trim();

    // Clear previous error
    errorEl.textContent = '';
    errorEl.classList.remove('visible');

    // Validate URL
    if (!url) {
        showError(errorEl, 'Please enter an addon URL');
        return;
    }

    // Basic URL validation
    try {
        new URL(url);
    } catch {
        showError(errorEl, 'Invalid URL format');
        return;
    }

    // Check if URL ends with manifest.json
    if (!url.endsWith('/manifest.json')) {
        showError(errorEl, 'URL must end with /manifest.json');
        return;
    }

    // Disable button during request
    addBtn.disabled = true;
    addBtn.textContent = 'Adding...';

    try {
        const response = await fetch('/api/addons', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
            },
            body: JSON.stringify({ manifestUrl: url }),
        });

        if (!response.ok) {
            const errorData = await response.json().catch(() => ({}));
            throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        // Success - clear input and reload
        urlInput.value = '';
        loadAddons();
    } catch (error) {
        console.error('Failed to add addon:', error);
        showError(errorEl, `Failed to add addon: ${error.message}`);
    } finally {
        addBtn.disabled = false;
        addBtn.textContent = 'Add';
    }
}

// Remove addon
async function removeAddon(id) {
    if (!confirm('Are you sure you want to remove this addon?')) {
        return;
    }

    try {
        const response = await fetch(`/api/addons/${id}`, {
            method: 'DELETE',
        });

        if (!response.ok) {
            const errorData = await response.json().catch(() => ({}));
            throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        // Success - reload list
        loadAddons();
    } catch (error) {
        console.error('Failed to remove addon:', error);
        alert(`Failed to remove addon: ${error.message}`);
    }
}

// Update per-addon fetch method
async function updateAddonFetchMethod(id, method) {
    try {
        const response = await fetch(`/api/addons/${id}`, {
            method: 'PATCH',
            headers: {
                'Content-Type': 'application/json',
            },
            body: JSON.stringify({ fetchMethod: method }),
        });

        if (!response.ok) {
            const errorData = await response.json().catch(() => ({}));
            throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        // Re-check if relay is needed with the new per-addon setting
        setTimeout(checkRelayNeeded, 500);
    } catch (error) {
        console.error('Failed to update addon fetch method:', error);
        alert(`Failed to update: ${error.message}`);
        // Reload to reset the select to the correct value
        loadAddons();
    }
}

// ---------------------------------------------------------------------------
// Cache Stats
// ---------------------------------------------------------------------------

// Load cache statistics from API
async function loadCacheStats() {
    const statsEl = document.getElementById('cache-stats');

    try {
        const response = await fetch('/api/cache/stats');
        if (!response.ok) {
            if (response.status === 404) {
                statsEl.innerHTML = '<div class="empty-state">Cache stats unavailable (API not implemented yet)</div>';
                return;
            }
            throw new Error(`HTTP ${response.status}`);
        }

        const stats = await response.json();

        const usedGB = stats.totalSizeGB || 0;
        const maxGB = stats.maxSizeGB || 0;
        const percent = maxGB > 0 ? Math.min((usedGB / maxGB) * 100, 100) : 0;
        const barClass = percent > 90 ? 'progress-danger' : (percent > 70 ? 'progress-warn' : '');

        statsEl.innerHTML = `
            <div class="cache-stats-grid">
                <div class="cache-stat-row">
                    <span class="cache-stat-label">Usage</span>
                    <span class="cache-stat-value">${usedGB.toFixed(2)} GB / ${maxGB} GB</span>
                </div>
                <div class="progress-bar-container">
                    <div class="progress-bar ${barClass}" style="width: ${percent.toFixed(1)}%"></div>
                </div>
                <div class="cache-stat-row">
                    <span class="cache-stat-label">Cached Torrents</span>
                    <span class="cache-stat-value">${stats.torrentCount || 0}</span>
                </div>
                <div class="cache-stat-row">
                    <span class="cache-stat-label">Max Age</span>
                    <span class="cache-stat-value">${stats.maxAgeDays || 0} days</span>
                </div>
                ${stats.oldestAccess ? `<div class="cache-stat-row">
                    <span class="cache-stat-label">Oldest Access</span>
                    <span class="cache-stat-value">${timeAgo(stats.oldestAccess)}</span>
                </div>` : ''}
            </div>
            <button class="cache-clean-btn" onclick="cleanupCache()">Clean Now</button>
        `;
    } catch (error) {
        console.error('Failed to load cache stats:', error);
        statsEl.innerHTML = '<div class="empty-state">Cache stats unavailable</div>';
    }
}

// Trigger manual cache cleanup
async function cleanupCache() {
    const statsEl = document.getElementById('cache-stats');
    const cleanBtn = statsEl.querySelector('.cache-clean-btn');

    if (cleanBtn) {
        cleanBtn.disabled = true;
        cleanBtn.textContent = 'Cleaning...';
    }

    try {
        const response = await fetch('/api/cache/cleanup', {
            method: 'POST',
        });

        if (!response.ok) {
            if (response.status === 404) {
                alert('Cache cleanup API not available yet');
                return;
            }
            const errorData = await response.json().catch(() => ({}));
            throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        // Refresh cache stats and torrents list after cleanup
        loadCacheStats();
        loadTorrents();
    } catch (error) {
        console.error('Cache cleanup failed:', error);
        alert(`Cache cleanup failed: ${error.message}`);
        if (cleanBtn) {
            cleanBtn.disabled = false;
            cleanBtn.textContent = 'Clean Now';
        }
    }
}

// ---------------------------------------------------------------------------
// Live Torrent Stats
// ---------------------------------------------------------------------------

// Load live torrent stats from engine (peers, speed, etc.)
async function loadLiveStats() {
    const listEl = document.getElementById('live-stats');
    const totalSpeedEl = document.getElementById('total-speed');

    try {
        const response = await fetch('/api/torrents/stats');
        if (!response.ok) {
            if (response.status === 404 || response.status === 503) {
                listEl.innerHTML = '<div class="empty-state">Engine stats unavailable</div>';
                totalSpeedEl.textContent = '';
                return;
            }
            throw new Error(`HTTP ${response.status}`);
        }

        const torrents = await response.json();

        if (!torrents || torrents.length === 0) {
            listEl.innerHTML = '<div class="empty-state">No active torrents</div>';
            totalSpeedEl.textContent = '';
            return;
        }

        // Calculate totals
        let totalDown = 0;
        let totalUp = 0;
        let totalPeers = 0;
        for (const t of torrents) {
            totalDown += t.downloadSpeed || 0;
            totalUp += t.uploadSpeed || 0;
            totalPeers += t.activePeers || 0;
        }

        totalSpeedEl.innerHTML = totalPeers > 0
            ? `${totalPeers} peers | ${formatSpeed(totalDown)} down | ${formatSpeed(totalUp)} up`
            : '';

        // Sort: torrents with active stats first, then by download speed
        torrents.sort((a, b) => {
            const aActive = (a.activePeers || 0) + (a.downloadSpeed || 0);
            const bActive = (b.activePeers || 0) + (b.downloadSpeed || 0);
            return bActive - aActive;
        });

        listEl.innerHTML = torrents.map(t => {
            const name = t.name || t.infoHash || 'Unknown';
            const truncName = name.length > 60 ? name.substring(0, 57) + '...' : name;
            const hasStats = (t.activePeers || 0) > 0 || (t.downloadSpeed || 0) > 0;

            return `
                <div class="live-torrent">
                    <div class="live-torrent-name" title="${escapeHtml(name)}">${escapeHtml(truncName)}</div>
                    <div class="live-torrent-stats">
                        <div class="live-stat">
                            <span class="live-stat-label">Peers:</span>
                            <span class="live-stat-value peers">${t.activePeers || 0} / ${t.totalPeers || 0}</span>
                        </div>
                        <div class="live-stat">
                            <span class="live-stat-label">Seeders:</span>
                            <span class="live-stat-value seeders">${t.connectedSeeders || 0}</span>
                        </div>
                        <div class="live-stat">
                            <span class="live-stat-label">DL:</span>
                            <span class="live-stat-value speed">${formatSpeed(t.downloadSpeed || 0)}</span>
                        </div>
                        <div class="live-stat">
                            <span class="live-stat-label">UL:</span>
                            <span class="live-stat-value speed">${formatSpeed(t.uploadSpeed || 0)}</span>
                        </div>
                        <div class="live-stat">
                            <span class="live-stat-label">Size:</span>
                            <span class="live-stat-value">${formatBytes(t.totalSize || 0)}</span>
                        </div>
                        ${!hasStats ? '<div class="live-stat"><span class="live-stat-label">(idle)</span></div>' : ''}
                    </div>
                </div>
            `;
        }).join('');
    } catch (error) {
        console.error('Failed to load live stats:', error);
        listEl.innerHTML = '<div class="empty-state">Engine stats unavailable</div>';
    }
}

// Remove a single cached torrent
async function removeTorrent(infoHash) {
    if (!confirm('Remove this torrent from cache?')) {
        return;
    }

    try {
        const response = await fetch(`/api/cache/torrents/${infoHash}`, {
            method: 'DELETE',
        });

        if (!response.ok) {
            if (response.status === 404) {
                alert('Torrent removal API not available yet');
                return;
            }
            const errorData = await response.json().catch(() => ({}));
            throw new Error(errorData.error || `HTTP ${response.status}`);
        }

        // Refresh stats
        loadCacheStats();
        loadLiveStats();
    } catch (error) {
        console.error('Failed to remove torrent:', error);
        alert(`Failed to remove torrent: ${error.message}`);
    }
}

// ---------------------------------------------------------------------------
// Clipboard
// ---------------------------------------------------------------------------

// Copy to clipboard with visual feedback
async function copyToClipboard(text) {
    try {
        await navigator.clipboard.writeText(text);

        // Show feedback
        const feedback = document.createElement('div');
        feedback.className = 'copied-feedback';
        feedback.textContent = 'Copied!';
        document.body.appendChild(feedback);

        // Remove after animation
        setTimeout(() => {
            document.body.removeChild(feedback);
        }, 2000);
    } catch (error) {
        console.error('Failed to copy:', error);
        alert('Failed to copy to clipboard');
    }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// Format bytes/sec to human-readable speed string
function formatSpeed(bytesPerSec) {
    if (bytesPerSec === 0) return '0 B/s';
    const units = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
    const k = 1024;
    const i = Math.min(Math.floor(Math.log(bytesPerSec) / Math.log(k)), units.length - 1);
    return (bytesPerSec / Math.pow(k, i)).toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
}

// Format bytes to human-readable string (KB, MB, GB)
function formatBytes(bytes) {
    if (bytes === 0) return '0 B';

    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const k = 1024;
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    const idx = Math.min(i, units.length - 1);

    return (bytes / Math.pow(k, idx)).toFixed(idx > 0 ? 2 : 0) + ' ' + units[idx];
}

// Format a date string to relative time ("2 hours ago", "3 days ago")
function timeAgo(dateString) {
    const date = new Date(dateString);
    const now = new Date();
    const diffMs = now - date;

    if (isNaN(diffMs)) return dateString;

    const seconds = Math.floor(diffMs / 1000);
    const minutes = Math.floor(seconds / 60);
    const hours = Math.floor(minutes / 60);
    const days = Math.floor(hours / 24);

    if (days > 0) return days === 1 ? '1 day ago' : `${days} days ago`;
    if (hours > 0) return hours === 1 ? '1 hour ago' : `${hours} hours ago`;
    if (minutes > 0) return minutes === 1 ? '1 minute ago' : `${minutes} minutes ago`;
    return 'just now';
}

// Show error message
function showError(element, message) {
    element.textContent = message;
    element.classList.add('visible');
}

// Show status message (auto-fades)
function showStatus(element, message, isError) {
    element.textContent = message;
    element.style.color = isError ? '#e94560' : '#4ec9b0';
    element.classList.add('visible');

    // Reset visibility after animation
    setTimeout(() => {
        element.classList.remove('visible');
    }, 3000);
}

// Extract a short label from an addon URL (domain or last path segment)
function extractAddonLabel(url) {
    try {
        const u = new URL(url);
        // Use hostname without TLD, e.g. "torrentio.strem.fun" -> "torrentio"
        const parts = u.hostname.split('.');
        return parts[0].charAt(0).toUpperCase() + parts[0].slice(1);
    } catch {
        return 'Unknown addon';
    }
}

// Escape HTML to prevent XSS
function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}
