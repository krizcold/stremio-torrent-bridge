// Auto-refresh interval handle
let cacheRefreshInterval = null;

// Initialize on page load
document.addEventListener('DOMContentLoaded', () => {
    loadConfig();
    loadAddons();
    loadCacheStats();
    loadTorrents();

    // Auto-refresh cache stats every 30 seconds
    cacheRefreshInterval = setInterval(() => {
        loadCacheStats();
        loadTorrents();
    }, 30000);
});

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
        document.getElementById('cache-size').value = config.cacheSizeGB || 50;
        document.getElementById('cache-age').value = config.cacheMaxAgeDays || 30;

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
            const displayName = addon.name || 'Loading...';
            const originalUrl = addon.originalUrl || '';
            const truncatedURL = originalUrl.length > 60
                ? originalUrl.substring(0, 57) + '...'
                : originalUrl;

            // Use wrappedUrl directly from API
            const wrappedURL = addon.wrappedUrl || '';

            return `
                <div class="addon-item">
                    <div class="addon-header">
                        <div>
                            <div class="addon-name">${escapeHtml(displayName)}</div>
                            <div class="addon-url" title="${escapeHtml(originalUrl)}">${escapeHtml(truncatedURL)}</div>
                        </div>
                        <div class="addon-actions">
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
// Active Torrents
// ---------------------------------------------------------------------------

// Load cached torrents list
async function loadTorrents() {
    const listEl = document.getElementById('torrents-list');

    try {
        const response = await fetch('/api/cache/stats');
        if (!response.ok) {
            if (response.status === 404) {
                listEl.innerHTML = '<div class="empty-state">Torrent list unavailable (API not implemented yet)</div>';
                return;
            }
            throw new Error(`HTTP ${response.status}`);
        }

        const stats = await response.json();
        const torrents = stats.torrents || [];

        if (torrents.length === 0) {
            listEl.innerHTML = '<div class="empty-state">No cached torrents</div>';
            return;
        }

        // Sort by last accessed, most recent first
        torrents.sort((a, b) => {
            const dateA = a.lastAccessed ? new Date(a.lastAccessed).getTime() : 0;
            const dateB = b.lastAccessed ? new Date(b.lastAccessed).getTime() : 0;
            return dateB - dateA;
        });

        let tableHTML = `
            <div class="torrents-table-wrapper">
                <table class="torrents-table">
                    <thead>
                        <tr>
                            <th>Name</th>
                            <th>Size</th>
                            <th>Last Accessed</th>
                            <th>Actions</th>
                        </tr>
                    </thead>
                    <tbody>
        `;

        for (const torrent of torrents) {
            const name = torrent.name || torrent.infoHash || 'Unknown';
            const truncatedName = name.length > 50 ? name.substring(0, 47) + '...' : name;

            tableHTML += `
                <tr>
                    <td title="${escapeHtml(name)}">${escapeHtml(truncatedName)}</td>
                    <td class="nowrap">${formatBytes(torrent.size || 0)}</td>
                    <td class="nowrap">${torrent.lastAccessed ? timeAgo(torrent.lastAccessed) : 'N/A'}</td>
                    <td>
                        <button class="small danger" onclick="removeTorrent('${escapeHtml(torrent.infoHash)}')">Remove</button>
                    </td>
                </tr>
            `;
        }

        tableHTML += '</tbody></table></div>';
        listEl.innerHTML = tableHTML;
    } catch (error) {
        console.error('Failed to load torrents:', error);
        listEl.innerHTML = '<div class="empty-state">Torrent list unavailable</div>';
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

        // Refresh both sections
        loadCacheStats();
        loadTorrents();
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

// Escape HTML to prevent XSS
function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}
