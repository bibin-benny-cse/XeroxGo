// XeroxGo Admin Dashboard Controller
(function () {
  'use strict';

  // --- Session & Auth State ---
  let authToken = localStorage.getItem('xeroxgo_admin_token') || '';

  // --- API Request Helper ---
  async function apiRequest(endpoint, options = {}) {
    const headers = {
      'Content-Type': 'application/json',
      ...(options.headers || {})
    };

    if (authToken) {
      headers['Authorization'] = `Bearer ${authToken}`;
    }

    const config = {
      ...options,
      headers
    };

    if (config.body && typeof config.body === 'object' && !(config.body instanceof FormData)) {
      config.body = JSON.stringify(config.body);
    }

    return fetch(endpoint, config);
  }

  // Helper to safely parse JSON responses
  async function parseResponseJSON(response) {
    const text = await response.text();
    if (!text || text.trim() === '') {
      return {};
    }
    try {
      return JSON.parse(text);
    } catch (_) {
      return { error: text.trim() };
    }
  }

  function removeAppLoader() {
    const loader = document.getElementById('app-loader');
    if (loader) loader.remove();
  }

  // --- Session Validation (Standard Initialization Gate: No FOUC / Login Flash) ---
  async function checkSession() {
    if (!authToken) {
      removeAppLoader();
      showAuthPage();
      return;
    }

    try {
      const res = await apiRequest('/api/admin/me');
      if (res.ok) {
        const userData = await parseResponseJSON(res);
        removeAppLoader();
        mountDashboard(userData);
      } else {
        localStorage.removeItem('xeroxgo_admin_token');
        authToken = '';
        removeAppLoader();
        showAuthPage();
      }
    } catch (_) {
      removeAppLoader();
      showAuthPage();
    }
  }

  function showAuthPage() {
    removeAppLoader();
    const authPage = document.getElementById('auth-page');
    if (authPage) authPage.classList.remove('hidden');

    const form = document.getElementById('login-form');
    const errBox = document.getElementById('login-error');
    const pwdInput = document.getElementById('login-password');
    const userInput = document.getElementById('login-username');
    const submitBtn = document.getElementById('login-submit-btn');

    if (!form || form._hasListener) return;
    form._hasListener = true;

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      errBox.classList.add('hidden');
      errBox.textContent = '';
      submitBtn.disabled = true;
      submitBtn.textContent = 'Signing in...';

      const username = userInput.value.trim();
      const password = pwdInput.value;

      try {
        const res = await apiRequest('/api/admin/login', {
          method: 'POST',
          body: { username, password }
        });

        const data = await parseResponseJSON(res);
        if (res.ok && data.token) {
          authToken = data.token;
          localStorage.setItem('xeroxgo_admin_token', authToken);
          pwdInput.value = '';
          authPage.classList.add('hidden');
          mountDashboard(data);
        } else {
          errBox.textContent = data.error || 'Invalid credentials';
          errBox.classList.remove('hidden');
          pwdInput.focus();
        }
      } catch (err) {
        errBox.textContent = 'Connection error: ' + err.message;
        errBox.classList.remove('hidden');
      } finally {
        submitBtn.disabled = false;
        submitBtn.textContent = 'Sign In';
      }
    });
  }

  // --- Mount Dashboard ---
  function mountDashboard(userData) {
    const authPage = document.getElementById('auth-page');
    if (authPage) authPage.classList.add('hidden');

    const template = document.getElementById('dashboard-template');
    if (!template) return;

    const clone = template.content.cloneNode(true);
    document.body.appendChild(clone);

    initDashboard(userData);
  }

  // --- Dashboard Controller ---
  function initDashboard(userData) {
    // --- Global State ---
    let activeNavTab = 'overview';
    let ads = [];
    let currentPreviewIdx = 0;
    let previewTimer = null;
    let activePreviewTab = 'mobile';
    let isEditingInModal = false;

    let currentBWSimplexPaise = 200;
    let currentBWDuplexPaise = 150;
    let currentColorSimplexPaise = 1000;
    let currentColorDuplexPaise = 750;
    let currentColorEnabled = false;

    let ordersFilter = {
      page: 0,
      limit: 25,
      total: 0,
      status: '',
      search: ''
    };

    // DOM Elements - Navigation & User
    const logoutBtn = document.getElementById('logout-btn');
    const userNameEl = document.getElementById('user-name');
    if (userNameEl && userData && userData.username) {
      userNameEl.textContent = userData.username;
    }

    const tabNavOverview = document.getElementById('tab-nav-overview');
    const tabNavOrders = document.getElementById('tab-nav-orders');
    const tabNavPricing = document.getElementById('tab-nav-pricing');
    const tabNavAds = document.getElementById('tab-nav-ads');

    const viewOverview = document.getElementById('view-overview');
    const viewOrders = document.getElementById('view-orders');
    const viewPricing = document.getElementById('view-pricing');
    const viewAds = document.getElementById('view-ads');

    // Toast element
    const toastEl = document.getElementById('toast');
    const toastMessageEl = document.getElementById('toast-message');

    function showToast(message) {
      if (!toastEl) return;
      toastMessageEl.textContent = message;
      toastEl.classList.remove('hidden');
      clearTimeout(toastEl._timer);
      toastEl._timer = setTimeout(() => {
        toastEl.classList.add('hidden');
      }, 3000);
    }

    // Sign out handler
    logoutBtn.addEventListener('click', async () => {
      try {
        await apiRequest('/api/admin/logout', { method: 'POST' });
      } catch (_) {}
      localStorage.removeItem('xeroxgo_admin_token');
      window.location.reload();
    });

    // --- Change Password Modal Controller ---
    const changePasswordNavBtn = document.getElementById('change-password-nav-btn');
    const passwordModal = document.getElementById('password-modal');
    const passwordModalCloseBtn = document.getElementById('password-modal-close-btn');
    const passwordModalCancelBtn = document.getElementById('password-modal-cancel-btn');
    const passwordForm = document.getElementById('password-form');
    const pwdCurrentInput = document.getElementById('pwd-current');
    const pwdNewInput = document.getElementById('pwd-new');
    const pwdConfirmInput = document.getElementById('pwd-confirm');
    const passwordModalError = document.getElementById('password-modal-error');
    const passwordModalSubmitBtn = document.getElementById('password-modal-submit-btn');

    function closePasswordModal() {
      if (!passwordModal) return;
      passwordModal.classList.add('hidden');
      if (passwordForm) passwordForm.reset();
      if (passwordModalError) {
        passwordModalError.classList.add('hidden');
        passwordModalError.textContent = '';
      }
    }

    if (changePasswordNavBtn && passwordModal) {
      changePasswordNavBtn.addEventListener('click', () => {
        closePasswordModal();
        passwordModal.classList.remove('hidden');
        if (pwdCurrentInput) pwdCurrentInput.focus();
      });
    }

    if (passwordModalCloseBtn) {
      passwordModalCloseBtn.addEventListener('click', closePasswordModal);
    }
    if (passwordModalCancelBtn) {
      passwordModalCancelBtn.addEventListener('click', closePasswordModal);
    }
    if (passwordModal) {
      passwordModal.addEventListener('click', (e) => {
        if (e.target === passwordModal) closePasswordModal();
      });
    }

    if (passwordForm) {
      passwordForm.addEventListener('submit', async (e) => {
        e.preventDefault();
        passwordModalError.classList.add('hidden');
        passwordModalError.textContent = '';

        const currentPassword = pwdCurrentInput.value;
        const newPassword = pwdNewInput.value;
        const confirmPassword = pwdConfirmInput.value;

        if (newPassword.length < 8) {
          passwordModalError.textContent = 'New password must be at least 8 characters long.';
          passwordModalError.classList.remove('hidden');
          pwdNewInput.focus();
          return;
        }

        if (newPassword !== confirmPassword) {
          passwordModalError.textContent = 'New passwords do not match.';
          passwordModalError.classList.remove('hidden');
          pwdConfirmInput.focus();
          return;
        }

        passwordModalSubmitBtn.disabled = true;
        passwordModalSubmitBtn.textContent = 'Updating...';

        try {
          const res = await apiRequest('/api/admin/password', {
            method: 'POST',
            body: {
              current_password: currentPassword,
              new_password: newPassword
            }
          });
          const data = await parseResponseJSON(res);
          if (res.ok && data.ok) {
            closePasswordModal();
            showToast('Password changed successfully. Please sign in again.');
            localStorage.removeItem('xeroxgo_admin_token');
            authToken = '';
            setTimeout(() => {
              window.location.reload();
            }, 1200);
          } else {
            passwordModalError.textContent = data.error || 'Failed to update password.';
            passwordModalError.classList.remove('hidden');
          }
        } catch (err) {
          passwordModalError.textContent = 'Error: ' + err.message;
          passwordModalError.classList.remove('hidden');
        } finally {
          passwordModalSubmitBtn.disabled = false;
          passwordModalSubmitBtn.textContent = 'Update Password';
        }
      });
    }

    // --- Tab Navigation ---
    function switchNavTab(tabName) {
      activeNavTab = tabName;
      [tabNavOverview, tabNavOrders, tabNavPricing, tabNavAds].forEach(btn => {
        if (btn) btn.classList.remove('active');
      });
      [viewOverview, viewOrders, viewPricing, viewAds].forEach(view => {
        if (view) view.classList.add('hidden');
      });

      if (tabName === 'overview') {
        if (tabNavOverview) tabNavOverview.classList.add('active');
        if (viewOverview) viewOverview.classList.remove('hidden');
        loadAnalytics();
      } else if (tabName === 'orders') {
        if (tabNavOrders) tabNavOrders.classList.add('active');
        if (viewOrders) viewOrders.classList.remove('hidden');
        loadOrders();
      } else if (tabName === 'pricing') {
        if (tabNavPricing) tabNavPricing.classList.add('active');
        if (viewPricing) viewPricing.classList.remove('hidden');
        loadPricing();
      } else if (tabName === 'ads') {
        if (tabNavAds) tabNavAds.classList.add('active');
        if (viewAds) viewAds.classList.remove('hidden');
        loadAds();
      }
    }

    if (tabNavOverview) tabNavOverview.addEventListener('click', () => switchNavTab('overview'));
    if (tabNavOrders) tabNavOrders.addEventListener('click', () => switchNavTab('orders'));
    if (tabNavPricing) tabNavPricing.addEventListener('click', () => switchNavTab('pricing'));
    if (tabNavAds) tabNavAds.addEventListener('click', () => switchNavTab('ads'));

    // ==========================================
    // 1. OVERVIEW & ANALYTICS
    // ==========================================
    const kpiRevenue = document.getElementById('kpi-revenue');
    const kpiRevenueToday = document.getElementById('kpi-revenue-today');
    const kpiRevenue7d = document.getElementById('kpi-revenue-7d');
    const kpiSheets = document.getElementById('kpi-sheets');
    const kpiBwOrders = document.getElementById('kpi-bw-orders');
    const kpiColorOrders = document.getElementById('kpi-color-orders');
    const kpiCompleted = document.getElementById('kpi-completed');
    const kpiTotalOrders = document.getElementById('kpi-total-orders');
    const kpiSuccessRate = document.getElementById('kpi-success-rate');
    const kpiAov = document.getElementById('kpi-aov');
    const analyticsChartContainer = document.getElementById('analytics-chart-container');

    async function loadAnalytics() {
      try {
        const res = await apiRequest('/api/admin/analytics?range=7d');
        if (!res.ok) throw new Error('Failed to load analytics');
        const data = await parseResponseJSON(res);

        if (kpiRevenue) kpiRevenue.textContent = `₹${(data.total_revenue_paise / 100).toLocaleString('en-IN', { minimumFractionDigits: 2 })}`;
        if (kpiRevenueToday) kpiRevenueToday.textContent = `₹${(data.revenue_today_paise / 100).toFixed(2)} today`;
        if (kpiRevenue7d) kpiRevenue7d.textContent = `₹${(data.revenue_7d_paise / 100).toFixed(2)} this week`;

        if (kpiSheets) kpiSheets.textContent = `${data.total_sheets_printed.toLocaleString()} sheets`;
        if (kpiBwOrders) kpiBwOrders.textContent = `${data.bw_orders_count} B&W`;
        if (kpiColorOrders) kpiColorOrders.textContent = `${data.color_orders_count} Color`;

        if (kpiCompleted) kpiCompleted.textContent = data.completed_orders.toLocaleString();
        if (kpiTotalOrders) kpiTotalOrders.textContent = `${data.total_orders.toLocaleString()} total attempts`;

        if (kpiSuccessRate) kpiSuccessRate.textContent = `${data.success_rate_percent.toFixed(1)}%`;
        if (kpiAov) kpiAov.textContent = `Avg ₹${(data.average_order_paise / 100).toFixed(2)} / order`;

        renderDailyChart(data.daily_trends || []);
      } catch (err) {
        if (analyticsChartContainer) {
          analyticsChartContainer.innerHTML = `<div style="text-align:center; padding:2rem; color:var(--text-muted);">Analytics loading error: ${escapeHTML(err.message)}</div>`;
        }
      }
    }

    function renderDailyChart(trends) {
      if (!analyticsChartContainer) return;
      if (!trends || trends.length === 0) {
        analyticsChartContainer.innerHTML = `<div style="text-align:center; padding:3rem; color:var(--text-muted);">No trend activity recorded yet.</div>`;
        return;
      }

      const maxRev = Math.max(...trends.map(t => t.revenue_paise / 100), 10);
      const width = 800;
      const height = 180;
      const padLeft = 45;
      const padRight = 20;
      const padTop = 20;
      const padBottom = 30;

      const innerWidth = width - padLeft - padRight;
      const innerHeight = height - padTop - padBottom;

      const pts = trends.map((t, i) => {
        const x = padLeft + (i / (trends.length - 1)) * innerWidth;
        const val = t.revenue_paise / 100;
        const y = padTop + innerHeight - (val / maxRev) * innerHeight;
        return { x, y, date: t.date, val, sheets: t.sheets_count, orders: t.order_count };
      });

      const lineD = pts.map((p, i) => `${i === 0 ? 'M' : 'L'} ${p.x.toFixed(1)} ${p.y.toFixed(1)}`).join(' ');
      const areaD = `${lineD} L ${pts[pts.length - 1].x.toFixed(1)} ${(padTop + innerHeight).toFixed(1)} L ${pts[0].x.toFixed(1)} ${(padTop + innerHeight).toFixed(1)} Z`;

      const svgHtml = `
        <svg viewBox="0 0 ${width} ${height}" style="width:100%; height:auto; overflow:visible;">
          <defs>
            <linearGradient id="chartGradient" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stop-color="#2563EB" stop-opacity="0.25" />
              <stop offset="100%" stop-color="#2563EB" stop-opacity="0.0" />
            </linearGradient>
          </defs>

          <!-- Horizontal Gridlines -->
          <line x1="${padLeft}" y1="${padTop}" x2="${width - padRight}" y2="${padTop}" stroke="#E2E8F0" stroke-dasharray="3,3" />
          <line x1="${padLeft}" y1="${padTop + innerHeight * 0.5}" x2="${width - padRight}" y2="${padTop + innerHeight * 0.5}" stroke="#E2E8F0" stroke-dasharray="3,3" />
          <line x1="${padLeft}" y1="${padTop + innerHeight}" x2="${width - padRight}" y2="${padTop + innerHeight}" stroke="#CBD5E1" />

          <!-- Y-Axis Labels -->
          <text x="${padLeft - 8}" y="${padTop + 4}" fill="#94A3B8" font-size="10" text-anchor="end" font-weight="600">₹${maxRev.toFixed(0)}</text>
          <text x="${padLeft - 8}" y="${padTop + innerHeight * 0.5 + 4}" fill="#94A3B8" font-size="10" text-anchor="end" font-weight="600">₹${(maxRev * 0.5).toFixed(0)}</text>
          <text x="${padLeft - 8}" y="${padTop + innerHeight + 4}" fill="#94A3B8" font-size="10" text-anchor="end" font-weight="600">₹0</text>

          <!-- Area & Line -->
          <path d="${areaD}" fill="url(#chartGradient)" />
          <path d="${lineD}" fill="none" stroke="#2563EB" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" />

          <!-- Data Points & X-Labels -->
          ${pts.map(p => `
            <circle cx="${p.x.toFixed(1)}" cy="${p.y.toFixed(1)}" r="4.5" fill="#FFFFFF" stroke="#2563EB" stroke-width="2.5">
              <title>${p.date}: ₹${p.val.toFixed(2)} (${p.sheets} sheets, ${p.orders} jobs)</title>
            </circle>
            <text x="${p.x.toFixed(1)}" y="${height - 6}" fill="#64748B" font-size="10" text-anchor="middle" font-weight="600">
              ${p.date.slice(5)}
            </text>
          `).join('')}
        </svg>
      `;

      analyticsChartContainer.innerHTML = svgHtml;
    }

    // ==========================================
    // 2. ORDER HISTORY & INSPECTION
    // ==========================================
    const ordersSearchInput = document.getElementById('orders-search-input');
    const ordersStatusFilter = document.getElementById('orders-status-filter');
    const ordersRefreshBtn = document.getElementById('orders-refresh-btn');
    const ordersExportBtn = document.getElementById('orders-export-btn');
    const ordersTableBody = document.getElementById('orders-table-body');
    const ordersCountText = document.getElementById('orders-count-text');
    const ordersPrevBtn = document.getElementById('orders-prev-btn');
    const ordersNextBtn = document.getElementById('orders-next-btn');

    const orderModal = document.getElementById('order-modal');
    const orderModalCloseBtn = document.getElementById('order-modal-close-btn');
    const orderModalBody = document.getElementById('order-modal-body');

    let searchDebounceTimer = null;
    if (ordersSearchInput) {
      ordersSearchInput.addEventListener('input', () => {
        clearTimeout(searchDebounceTimer);
        searchDebounceTimer = setTimeout(() => {
          ordersFilter.search = ordersSearchInput.value.trim();
          ordersFilter.page = 0;
          loadOrders();
        }, 300);
      });
    }

    if (ordersStatusFilter) {
      ordersStatusFilter.addEventListener('change', () => {
        ordersFilter.status = ordersStatusFilter.value;
        ordersFilter.page = 0;
        loadOrders();
      });
    }

    if (ordersRefreshBtn) {
      ordersRefreshBtn.addEventListener('click', () => loadOrders());
    }

    if (ordersPrevBtn) {
      ordersPrevBtn.addEventListener('click', () => {
        if (ordersFilter.page > 0) {
          ordersFilter.page--;
          loadOrders();
        }
      });
    }

    if (ordersNextBtn) {
      ordersNextBtn.addEventListener('click', () => {
        if ((ordersFilter.page + 1) * ordersFilter.limit < ordersFilter.total) {
          ordersFilter.page++;
          loadOrders();
        }
      });
    }

    if (ordersExportBtn) {
      ordersExportBtn.addEventListener('click', async () => {
        try {
          showToast('Preparing CSV export...');
          const res = await apiRequest('/api/admin/orders/export');
          if (!res.ok) throw new Error('Export failed');
          const blob = await res.blob();
          const url = window.URL.createObjectURL(blob);
          const a = document.createElement('a');
          a.href = url;
          a.download = `xeroxgo-orders-${new Date().toISOString().slice(0, 10)}.csv`;
          document.body.appendChild(a);
          a.click();
          a.remove();
          window.URL.revokeObjectURL(url);
          showToast('CSV downloaded');
        } catch (err) {
          alert('Export error: ' + err.message);
        }
      });
    }

    if (orderModalCloseBtn && orderModal) {
      orderModalCloseBtn.addEventListener('click', () => orderModal.classList.add('hidden'));
      orderModal.addEventListener('click', (e) => {
        if (e.target === orderModal) orderModal.classList.add('hidden');
      });
    }

    let loadedOrdersMap = {};

    async function loadOrders() {
      if (!ordersTableBody) return;
      ordersTableBody.innerHTML = `<tr><td colspan="8" style="text-align:center; padding:2rem; color:var(--text-muted);"><div class="spinner" style="margin:0 auto 0.75rem;"></div>Loading orders...</td></tr>`;

      try {
        const offset = ordersFilter.page * ordersFilter.limit;
        const query = new URLSearchParams({
          limit: ordersFilter.limit.toString(),
          offset: offset.toString(),
          status: ordersFilter.status,
          search: ordersFilter.search
        });

        const res = await apiRequest(`/api/admin/orders?${query.toString()}`);
        if (!res.ok) throw new Error('Failed to load orders');
        const data = await parseResponseJSON(res);

        const orders = data.orders || [];
        ordersFilter.total = data.total || 0;
        loadedOrdersMap = {};

        if (ordersCountText) {
          const from = ordersFilter.total === 0 ? 0 : offset + 1;
          const to = Math.min(offset + orders.length, ordersFilter.total);
          ordersCountText.textContent = `Showing ${from}–${to} of ${ordersFilter.total} orders`;
        }

        if (ordersPrevBtn) ordersPrevBtn.disabled = ordersFilter.page === 0;
        if (ordersNextBtn) ordersNextBtn.disabled = (ordersFilter.page + 1) * ordersFilter.limit >= ordersFilter.total;

        if (orders.length === 0) {
          ordersTableBody.innerHTML = `<tr><td colspan="8" style="text-align:center; padding:3rem 1rem; color:var(--text-muted);">No matching orders found.</td></tr>`;
          return;
        }

        ordersTableBody.innerHTML = orders.map(ord => {
          loadedOrdersMap[ord.order_id] = ord;
          const isColor = ord.color_mode === 'COLOR';
          const isCompleted = ord.status === 'COMPLETED';
          const isFailed = ord.status === 'FAILED_REFUNDABLE' || ord.status === 'PAUSED_ERROR';
          const isPrinting = ord.status === 'PRINTING' || ord.status === 'SPOOLING';

          let statusClass = 'badge-status-pending';
          if (isCompleted) statusClass = 'badge-status-completed';
          else if (isFailed) statusClass = 'badge-status-failed';
          else if (isPrinting) statusClass = 'badge-status-printing';

          const dateStr = new Date(ord.created_at).toLocaleString('en-IN', {
            month: 'short',
            day: 'numeric',
            hour: '2-digit',
            minute: '2-digit'
          });

          return `
            <tr>
              <td style="font-family:monospace; font-weight:600; font-size:0.8125rem;">${escapeHTML(ord.order_id)}</td>
              <td><span style="font-weight:700; color:var(--primary); font-family:monospace;">${escapeHTML(ord.display_code)}</span></td>
              <td style="color:var(--text-muted);">${dateStr}</td>
              <td>
                <span class="badge-tag" style="background:${isColor ? '#FEF3C7; color:#B45309;' : '#F1F5F9; color:#475569;'} font-size:0.75rem;">
                  ${isColor ? 'Color' : 'B&W'}
                </span>
              </td>
              <td>${ord.total_pages} pages (${ord.total_sheets} sheets)</td>
              <td style="font-weight:700;">₹${(ord.price_paise / 100).toFixed(2)}</td>
              <td><span class="badge-status ${statusClass}">${escapeHTML(ord.status)}</span></td>
              <td style="text-align:right;">
                <button class="btn-secondary btn-inspect-order" data-id="${ord.order_id}" type="button" style="padding:0.3rem 0.65rem; font-size:0.78rem;">
                  Inspect
                </button>
              </td>
            </tr>
          `;
        }).join('');

        ordersTableBody.querySelectorAll('.btn-inspect-order').forEach(btn => {
          btn.addEventListener('click', () => {
            const id = btn.dataset.id;
            const ord = loadedOrdersMap[id];
            if (ord) openOrderDetails(ord);
          });
        });

      } catch (err) {
        ordersTableBody.innerHTML = `<tr><td colspan="8" style="text-align:center; padding:2rem; color:var(--danger);">Error: ${escapeHTML(err.message)}</td></tr>`;
      }
    }

    function openOrderDetails(ord) {
      if (!orderModal || !orderModalBody) return;
      const amount = (ord.price_paise / 100).toFixed(2);
      const isColor = ord.color_mode === 'COLOR';

      orderModalBody.innerHTML = `
        <div style="display:grid; grid-template-columns:1fr 1fr; gap:0.75rem; padding-bottom:0.75rem; border-bottom:1px solid var(--border);">
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Order ID</span>
            <span style="font-family:monospace; font-weight:600;">${escapeHTML(ord.order_id)}</span>
          </div>
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Display Code</span>
            <span style="font-family:monospace; font-weight:800; font-size:1.1rem; color:var(--primary);">${escapeHTML(ord.display_code)}</span>
          </div>
        </div>

        <div style="display:grid; grid-template-columns:1fr 1fr; gap:0.75rem; padding-bottom:0.75rem; border-bottom:1px solid var(--border);">
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Job ID</span>
            <span style="font-family:monospace;">${escapeHTML(ord.job_id)}</span>
          </div>
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Kiosk</span>
            <span style="font-weight:600;">${escapeHTML(ord.kiosk_id)}</span>
          </div>
        </div>

        <div style="display:grid; grid-template-columns:1fr 1fr 1fr; gap:0.75rem; padding-bottom:0.75rem; border-bottom:1px solid var(--border);">
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Print Mode</span>
            <span style="font-weight:600;">${isColor ? 'Color' : 'Monochrome'}</span>
          </div>
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Pages / Sheets</span>
            <span style="font-weight:600;">${ord.total_pages} / ${ord.total_sheets}</span>
          </div>
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Duplex</span>
            <span style="font-weight:600;">${escapeHTML(ord.duplex)}</span>
          </div>
        </div>

        <div style="display:grid; grid-template-columns:1fr 1fr; gap:0.75rem; padding-bottom:0.75rem; border-bottom:1px solid var(--border);">
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Amount Charged</span>
            <span style="font-weight:800; font-size:1.2rem; color:var(--text-main);">₹${amount}</span>
          </div>
          <div>
            <span style="font-size:0.75rem; color:var(--text-muted); display:block;">Status</span>
            <span style="font-weight:700;">${escapeHTML(ord.status)}</span>
          </div>
        </div>

        ${ord.error_reason ? `
          <div style="padding:0.75rem; background:var(--danger-bg); border-radius:var(--radius-sm); border:1px solid #FECACA; color:var(--danger);">
            <strong>Error Message:</strong> ${escapeHTML(ord.error_reason)}
          </div>
        ` : ''}

        <div style="font-size:0.75rem; color:var(--text-muted); margin-top:0.5rem;">
          Created: ${new Date(ord.created_at).toUTCString()}
        </div>
      `;

      orderModal.classList.remove('hidden');
    }

    // ==========================================
    // 3. PRICING CONTROLLER & COST ESTIMATOR
    // ==========================================
    const pricingForm = document.getElementById('pricing-form');
    const priceBwSimplexInput = document.getElementById('price-bw-simplex-input');
    const priceBwDuplexInput = document.getElementById('price-bw-duplex-input');
    const priceColorSimplexInput = document.getElementById('price-color-simplex-input');
    const priceColorDuplexInput = document.getElementById('price-color-duplex-input');
    const pricingColorEnabledToggle = document.getElementById('pricing-color-enabled-toggle');
    const pricingSaveBtn = document.getElementById('pricing-save-btn');

    const calcPagesInput = document.getElementById('calc-pages-input');
    const calcResultSheets = document.getElementById('calc-result-sheets');
    const calcResultBw = document.getElementById('calc-result-bw');
    const calcResultColor = document.getElementById('calc-result-color');

    async function loadPricing() {
      try {
        const res = await apiRequest('/api/admin/pricing');
        if (!res.ok) throw new Error('Failed to load pricing');
        const data = await parseResponseJSON(res);

        currentBWSimplexPaise = data.bw_simplex_price_paise || data.bw_price_per_sheet_paise || 200;
        currentBWDuplexPaise = data.bw_duplex_price_paise || 150;
        currentColorSimplexPaise = data.color_simplex_price_paise || data.color_price_per_sheet_paise || 1000;
        currentColorDuplexPaise = data.color_duplex_price_paise || 750;
        currentColorEnabled = Boolean(data.color_enabled);

        if (priceBwSimplexInput) priceBwSimplexInput.value = (currentBWSimplexPaise / 100).toFixed(2);
        if (priceBwDuplexInput) priceBwDuplexInput.value = (currentBWDuplexPaise / 100).toFixed(2);
        if (priceColorSimplexInput) priceColorSimplexInput.value = (currentColorSimplexPaise / 100).toFixed(2);
        if (priceColorDuplexInput) priceColorDuplexInput.value = (currentColorDuplexPaise / 100).toFixed(2);
        if (pricingColorEnabledToggle) pricingColorEnabledToggle.checked = currentColorEnabled;

        updateCostEstimator();
      } catch (err) {
        showToast('Pricing load error: ' + err.message);
      }
    }

    function updateCostEstimator() {
      if (!calcPagesInput) return;
      const pages = Math.max(1, parseInt(calcPagesInput.value, 10) || 1);
      const isDuplex = document.querySelector('input[name="calc-duplex"]:checked')?.value === 'duplex';
      const sheets = isDuplex ? Math.ceil(pages / 2) : pages;

      const bwRate = isDuplex
        ? (priceBwDuplexInput && priceBwDuplexInput.value ? parseFloat(priceBwDuplexInput.value) : (currentBWDuplexPaise / 100))
        : (priceBwSimplexInput && priceBwSimplexInput.value ? parseFloat(priceBwSimplexInput.value) : (currentBWSimplexPaise / 100));

      const colorRate = isDuplex
        ? (priceColorDuplexInput && priceColorDuplexInput.value ? parseFloat(priceColorDuplexInput.value) : (currentColorDuplexPaise / 100))
        : (priceColorSimplexInput && priceColorSimplexInput.value ? parseFloat(priceColorSimplexInput.value) : (currentColorSimplexPaise / 100));

      const bwCost = pages * bwRate;
      const colorCost = pages * colorRate;

      if (calcResultSheets) calcResultSheets.textContent = `${sheets} sheet${sheets > 1 ? 's' : ''}`;
      if (calcResultBw) calcResultBw.textContent = `₹${bwCost.toFixed(2)}`;
      if (calcResultColor) calcResultColor.textContent = `₹${colorCost.toFixed(2)}`;
    }

    if (calcPagesInput) calcPagesInput.addEventListener('input', updateCostEstimator);
    document.querySelectorAll('input[name="calc-duplex"]').forEach(r => {
      r.addEventListener('change', updateCostEstimator);
    });
    if (priceBwSimplexInput) priceBwSimplexInput.addEventListener('input', updateCostEstimator);
    if (priceBwDuplexInput) priceBwDuplexInput.addEventListener('input', updateCostEstimator);
    if (priceColorSimplexInput) priceColorSimplexInput.addEventListener('input', updateCostEstimator);
    if (priceColorDuplexInput) priceColorDuplexInput.addEventListener('input', updateCostEstimator);
    if (pricingColorEnabledToggle) pricingColorEnabledToggle.addEventListener('change', updateCostEstimator);

    if (pricingForm) {
      pricingForm.addEventListener('submit', async (e) => {
        e.preventDefault();
        const bwSimplex = parseFloat(priceBwSimplexInput.value);
        const bwDuplex = parseFloat(priceBwDuplexInput.value);
        const colorSimplex = parseFloat(priceColorSimplexInput.value);
        const colorDuplex = parseFloat(priceColorDuplexInput.value);
        const colorEnabled = pricingColorEnabledToggle ? pricingColorEnabledToggle.checked : false;

        if (isNaN(bwSimplex) || bwSimplex <= 0 ||
            isNaN(bwDuplex) || bwDuplex <= 0 ||
            isNaN(colorSimplex) || colorSimplex <= 0 ||
            isNaN(colorDuplex) || colorDuplex <= 0) {
          alert('All prices must be valid positive numbers.');
          return;
        }

        pricingSaveBtn.disabled = true;
        pricingSaveBtn.textContent = 'Saving...';

        try {
          const res = await apiRequest('/api/admin/pricing', {
            method: 'PUT',
            body: {
              bw_simplex_price_paise: Math.round(bwSimplex * 100),
              bw_duplex_price_paise: Math.round(bwDuplex * 100),
              color_simplex_price_paise: Math.round(colorSimplex * 100),
              color_duplex_price_paise: Math.round(colorDuplex * 100),
              color_enabled: colorEnabled
            }
          });

          if (!res.ok) {
            const data = await parseResponseJSON(res);
            throw new Error(data.error || 'Failed to save');
          }

          currentBWSimplexPaise = Math.round(bwSimplex * 100);
          currentBWDuplexPaise = Math.round(bwDuplex * 100);
          currentColorSimplexPaise = Math.round(colorSimplex * 100);
          currentColorDuplexPaise = Math.round(colorDuplex * 100);
          currentColorEnabled = colorEnabled;

          updateCostEstimator();
          showToast('Print rates saved');
        } catch (err) {
          alert('Save error: ' + err.message);
        } finally {
          pricingSaveBtn.disabled = false;
          pricingSaveBtn.textContent = 'Save Rates';
        }
      });
    }

    // ==========================================
    // 4. PROMOTIONAL ADS & SIMULATOR
    // ==========================================
    const SYSTEM_DEFAULT_CARDS = [
      {
        id: 'ad-quick-easy',
        title: 'Quick & Easy',
        description: 'Direct UPI payment. No coins, no cash needed.',
        type: 'gradient',
        gradient_c0: '#1E3A8A',
        gradient_c1: '#2563EB',
        gradient_c2: '#4F46E5'
      },
      {
        id: 'ad-high-quality',
        title: 'High Quality Prints',
        description: 'Crisp laser documents ready in seconds.',
        type: 'gradient',
        gradient_c0: '#065F46',
        gradient_c1: '#10B981',
        gradient_c2: '#059669'
      },
      {
        id: 'ad-skip-queue',
        title: 'Skip the Xerox Line',
        description: 'Upload right from your phone and collect instantly.',
        type: 'gradient',
        gradient_c0: '#581C87',
        gradient_c1: '#7C3AED',
        gradient_c2: '#D97706'
      }
    ];

    const adListEl = document.getElementById('ad-list');
    const createAdBtn = document.getElementById('create-ad-btn');

    const tabMobileBtn = document.getElementById('tab-mobile');
    const tabKioskBtn = document.getElementById('tab-kiosk');
    const mobilePreviewContainer = document.getElementById('mobile-preview-container');
    const kioskPreviewContainer = document.getElementById('kiosk-preview-container');

    const mobileSlidePreview = document.getElementById('mobile-slide-preview');
    const mobilePreviewTitle = document.getElementById('mobile-preview-title');
    const mobilePreviewDesc = document.getElementById('mobile-preview-desc');
    const mobilePreviewDots = document.getElementById('mobile-preview-dots');

    const kioskSlidePreview = document.getElementById('kiosk-slide-preview');
    const kioskPreviewTitle = document.getElementById('kiosk-preview-title');
    const kioskPreviewDesc = document.getElementById('kiosk-preview-desc');

    const adModal = document.getElementById('ad-modal');
    const modalHeading = document.getElementById('modal-heading');
    const modalCloseBtn = document.getElementById('modal-close-btn');
    const modalCancelBtn = document.getElementById('modal-cancel-btn');
    const adForm = document.getElementById('ad-form');
    const adIdInput = document.getElementById('ad-id');
    const adTitleInput = document.getElementById('ad-title');
    const adDescInput = document.getElementById('ad-desc');
    const adLinkInput = document.getElementById('ad-link');
    const adIsActiveInput = document.getElementById('ad-is-active');

    const gradientControlsGroup = document.getElementById('gradient-controls-group');
    const imageControlsGroup = document.getElementById('image-controls-group');

    const adC0 = document.getElementById('ad-c0');
    const adC0Hex = document.getElementById('ad-c0-hex');
    const adC1 = document.getElementById('ad-c1');
    const adC1Hex = document.getElementById('ad-c1-hex');
    const adC2 = document.getElementById('ad-c2');
    const adC2Hex = document.getElementById('ad-c2-hex');

    const adImageFile = document.getElementById('ad-image-file');
    const uploadImageBtn = document.getElementById('upload-image-btn');
    const imagePreviewThumb = document.getElementById('image-preview-thumb');
    const adImageUrlInput = document.getElementById('ad-image-url');

    async function loadAds() {
      if (!adListEl) return;
      try {
        const res = await apiRequest('/api/admin/ads');
        if (!res.ok) throw new Error('Failed to load ads');
        const data = await parseResponseJSON(res);
        ads = Array.isArray(data) ? data : (data.ads || []);
        renderAdList();
        initPreviewCarousel();
      } catch (err) {
        adListEl.innerHTML = `
          <div style="text-align:center; padding:2rem; color:var(--danger);">
            Error loading ads: ${escapeHTML(err.message)}
            <br><button class="btn-secondary" style="margin-top:1rem;" onclick="location.reload()">Retry</button>
          </div>`;
      }
    }

    function renderAdList() {
      if (!adListEl) return;
      if (!ads || ads.length === 0) {
        adListEl.innerHTML = `
          <div class="ad-empty-box">
            <p style="font-size:0.95rem; font-weight:600; color:var(--text-main); margin-bottom:1rem;">No ads configured</p>
            <button class="btn-primary" type="button" onclick="document.getElementById('create-ad-btn').click()" style="margin:0 auto;">
              + New Ad
            </button>
          </div>`;
        return;
      }

      adListEl.innerHTML = ads.map((ad, idx) => {
        const isGradient = ad.type === 'gradient' || !ad.type;
        const bgStyle = isGradient
          ? `background: linear-gradient(135deg, ${ad.gradient_c0 || '#1E3A8A'}, ${ad.gradient_c1 || '#2563EB'}, ${ad.gradient_c2 || '#4F46E5'});`
          : `background: url('${escapeHTML(ad.image_url)}') center/cover no-repeat;`;

        return `
          <div class="ad-card-item ${ad.is_active ? '' : 'inactive'}" data-id="${ad.id}" data-index="${idx}">
            <div class="ad-drag-handle" title="Reorder">
              <div style="display:flex; flex-direction:column; gap:2px;">
                <button class="btn-move-up btn-icon" style="width:24px; height:20px; font-size:10px; border:none;" data-index="${idx}" ${idx === 0 ? 'disabled style="opacity:0.2"' : ''}>▲</button>
                <button class="btn-move-down btn-icon" style="width:24px; height:20px; font-size:10px; border:none;" data-index="${idx}" ${idx === ads.length - 1 ? 'disabled style="opacity:0.2"' : ''}>▼</button>
              </div>
            </div>

            <div class="ad-swatch-thumb" style="${bgStyle}">
              ${isGradient ? '' : '<span style="background:rgba(0,0,0,0.4); padding:2px 6px; border-radius:4px; font-size:9px;">IMG</span>'}
            </div>

            <div class="ad-info">
              <div class="ad-info-title">${escapeHTML(ad.title)}</div>
              <div class="ad-info-desc">${escapeHTML(ad.description)}</div>
              <div class="ad-badges">
                <span class="badge-tag ${isGradient ? 'badge-gradient' : 'badge-image'}">${isGradient ? 'Gradient' : 'Image'}</span>
                ${ad.link_url ? `<span class="badge-tag" style="background:#F1F5F9; color:#475569;" title="${escapeHTML(ad.link_url)}">Link</span>` : ''}
                ${ad.is_active ? '<span class="badge-tag" style="background:#ECFDF5; color:#065F46;">Active</span>' : '<span class="badge-tag" style="background:#FEF2F2; color:#991B1B;">Disabled</span>'}
              </div>
            </div>

            <div class="ad-controls">
              <label class="switch" title="Toggle active status">
                <input type="checkbox" class="toggle-active" data-id="${ad.id}" ${ad.is_active ? 'checked' : ''} />
                <span class="slider"></span>
              </label>
              <button class="btn-icon btn-edit" title="Edit" data-id="${ad.id}">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z"/></svg>
              </button>
              <button class="btn-icon delete btn-delete" title="Delete" data-id="${ad.id}">
                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
              </button>
            </div>
          </div>
        `;
      }).join('');

      // Wire active toggle listeners
      adListEl.querySelectorAll('.toggle-active').forEach(chk => {
        chk.addEventListener('change', async (e) => {
          const id = e.target.dataset.id;
          const active = e.target.checked;
          await updateAdActiveState(id, active);
        });
      });

      // Wire edit buttons
      adListEl.querySelectorAll('.btn-edit').forEach(btn => {
        btn.addEventListener('click', () => {
          const id = btn.dataset.id;
          const ad = ads.find(a => a.id === id);
          if (ad) openEditModal(ad);
        });
      });

      // Wire delete buttons
      adListEl.querySelectorAll('.btn-delete').forEach(btn => {
        btn.addEventListener('click', async () => {
          const id = btn.dataset.id;
          const ad = ads.find(a => a.id === id);
          if (ad && confirm(`Delete ad "${ad.title}"?`)) {
            await deleteAd(id);
          }
        });
      });

      // Wire move up
      adListEl.querySelectorAll('.btn-move-up').forEach(btn => {
        btn.addEventListener('click', async () => {
          const idx = parseInt(btn.dataset.index, 10);
          if (idx > 0) {
            const temp = ads[idx];
            ads[idx] = ads[idx - 1];
            ads[idx - 1] = temp;
            await saveReorder();
          }
        });
      });

      // Wire move down
      adListEl.querySelectorAll('.btn-move-down').forEach(btn => {
        btn.addEventListener('click', async () => {
          const idx = parseInt(btn.dataset.index, 10);
          if (idx < ads.length - 1) {
            const temp = ads[idx];
            ads[idx] = ads[idx + 1];
            ads[idx + 1] = temp;
            await saveReorder();
          }
        });
      });
    }

    async function updateAdActiveState(id, isActive) {
      try {
        const ad = ads.find(a => a.id === id);
        if (!ad) return;
        ad.is_active = isActive;

        const res = await apiRequest(`/api/admin/ads/${encodeURIComponent(id)}`, {
          method: 'PUT',
          body: ad
        });

        if (!res.ok) throw new Error('Failed to update status');
        showToast('Ad updated');
        renderAdList();
        initPreviewCarousel();
      } catch (err) {
        showToast('Error: ' + err.message);
        await loadAds();
      }
    }

    async function deleteAd(id) {
      try {
        const res = await apiRequest(`/api/admin/ads/${encodeURIComponent(id)}`, {
          method: 'DELETE'
        });
        if (!res.ok) throw new Error('Failed to delete ad');
        showToast('Ad deleted');
        await loadAds();
      } catch (err) {
        showToast('Error: ' + err.message);
      }
    }

    async function saveReorder() {
      try {
        const order = ads.map(a => a.id);
        const res = await apiRequest('/api/admin/ads/reorder', {
          method: 'POST',
          body: { order }
        });
        if (!res.ok) throw new Error('Failed to save order');
        showToast('Order saved');
        renderAdList();
        initPreviewCarousel();
      } catch (err) {
        showToast('Error: ' + err.message);
        await loadAds();
      }
    }

    // --- Preview Simulator ---
    if (tabMobileBtn && tabKioskBtn) {
      tabMobileBtn.addEventListener('click', () => {
        activePreviewTab = 'mobile';
        tabMobileBtn.classList.add('active');
        tabKioskBtn.classList.remove('active');
        mobilePreviewContainer.classList.remove('hidden');
        kioskPreviewContainer.classList.add('hidden');
      });

      tabKioskBtn.addEventListener('click', () => {
        activePreviewTab = 'kiosk';
        tabKioskBtn.classList.add('active');
        tabMobileBtn.classList.remove('active');
        kioskPreviewContainer.classList.remove('hidden');
        mobilePreviewContainer.classList.add('hidden');
      });
    }

    function initPreviewCarousel() {
      if (previewTimer) clearInterval(previewTimer);
      const activeCustomAds = ads.filter(a => a.is_active);
      const displayAds = activeCustomAds.length > 0 ? activeCustomAds : SYSTEM_DEFAULT_CARDS;

      currentPreviewIdx = 0;
      renderPreviewSlide(displayAds[currentPreviewIdx]);

      if (displayAds.length > 1) {
        previewTimer = setInterval(() => {
          if (!isEditingInModal) {
            currentPreviewIdx = (currentPreviewIdx + 1) % displayAds.length;
            renderPreviewSlide(displayAds[currentPreviewIdx]);
          }
        }, 5000);
      }
    }

    function renderPreviewSlide(ad) {
      if (!ad) return;
      const isGradient = ad.type === 'gradient' || !ad.type;
      const bg = isGradient
        ? `linear-gradient(135deg, ${ad.gradient_c0 || '#1E3A8A'} 0%, ${ad.gradient_c1 || '#2563EB'} 50%, ${ad.gradient_c2 || '#4F46E5'} 100%)`
        : `url('${ad.image_url}') center/cover no-repeat`;

      if (mobileSlidePreview) {
        mobileSlidePreview.style.background = bg;
        if (mobilePreviewTitle) mobilePreviewTitle.textContent = ad.title || 'Headline';
        if (mobilePreviewDesc) mobilePreviewDesc.textContent = ad.description || 'Subtitle';
      }

      if (kioskSlidePreview) {
        kioskSlidePreview.style.background = bg;
        if (kioskPreviewTitle) kioskPreviewTitle.textContent = ad.title || 'Headline';
        if (kioskPreviewDesc) kioskPreviewDesc.textContent = ad.description || 'Subtitle';
      }

      const activeCustomAds = ads.filter(a => a.is_active);
      const displayAds = activeCustomAds.length > 0 ? activeCustomAds : SYSTEM_DEFAULT_CARDS;
      if (mobilePreviewDots) {
        mobilePreviewDots.innerHTML = displayAds.map((_, i) =>
          `<span class="preview-dot ${i === currentPreviewIdx ? 'active' : ''}"></span>`
        ).join('');
      }
    }

    // Modal Form handlers
    function openCreateModal() {
      isEditingInModal = true;
      if (modalHeading) modalHeading.textContent = 'New Ad';
      if (adIdInput) adIdInput.value = '';
      if (adTitleInput) adTitleInput.value = '';
      if (adDescInput) adDescInput.value = '';
      if (adLinkInput) adLinkInput.value = '';
      if (adIsActiveInput) adIsActiveInput.checked = true;

      setFormType('gradient');
      setC0('#1E3A8A');
      setC1('#2563EB');
      setC2('#4F46E5');

      if (adImageUrlInput) adImageUrlInput.value = '';
      if (imagePreviewThumb) {
        imagePreviewThumb.style.backgroundImage = '';
        imagePreviewThumb.textContent = 'No file';
      }

      if (adModal) adModal.classList.remove('hidden');
      syncFormToLivePreview();
      if (adTitleInput) adTitleInput.focus();
    }

    function openEditModal(ad) {
      isEditingInModal = true;
      if (modalHeading) modalHeading.textContent = 'Edit Ad';
      if (adIdInput) adIdInput.value = ad.id;
      if (adTitleInput) adTitleInput.value = ad.title;
      if (adDescInput) adDescInput.value = ad.description;
      if (adLinkInput) adLinkInput.value = ad.link_url || '';
      if (adIsActiveInput) adIsActiveInput.checked = ad.is_active;

      setFormType(ad.type || 'gradient');
      setC0(ad.gradient_c0 || '#1E3A8A');
      setC1(ad.gradient_c1 || '#2563EB');
      setC2(ad.gradient_c2 || '#4F46E5');

      if (adImageUrlInput) adImageUrlInput.value = ad.image_url || '';
      if (imagePreviewThumb) {
        if (ad.image_url) {
          imagePreviewThumb.style.backgroundImage = `url('${ad.image_url}')`;
          imagePreviewThumb.textContent = '';
        } else {
          imagePreviewThumb.style.backgroundImage = '';
          imagePreviewThumb.textContent = 'No file';
        }
      }

      if (adModal) adModal.classList.remove('hidden');
      syncFormToLivePreview();
    }

    function closeModal() {
      isEditingInModal = false;
      if (adModal) adModal.classList.add('hidden');
      initPreviewCarousel();
    }

    if (createAdBtn) createAdBtn.addEventListener('click', openCreateModal);
    if (modalCloseBtn) modalCloseBtn.addEventListener('click', closeModal);
    if (modalCancelBtn) modalCancelBtn.addEventListener('click', closeModal);
    if (adModal) {
      adModal.addEventListener('click', (e) => {
        if (e.target === adModal) closeModal();
      });
    }

    function setFormType(type) {
      const radio = document.querySelector(`input[name="ad-type"][value="${type}"]`);
      if (radio) radio.checked = true;

      if (type === 'image') {
        if (gradientControlsGroup) gradientControlsGroup.classList.add('hidden');
        if (imageControlsGroup) imageControlsGroup.classList.remove('hidden');
      } else {
        if (gradientControlsGroup) gradientControlsGroup.classList.remove('hidden');
        if (imageControlsGroup) imageControlsGroup.classList.add('hidden');
      }
    }

    document.querySelectorAll('input[name="ad-type"]').forEach(r => {
      r.addEventListener('change', () => {
        setFormType(r.value);
        syncFormToLivePreview();
      });
    });

    function setC0(c) { if (adC0) adC0.value = c; if (adC0Hex) adC0Hex.value = c; }
    function setC1(c) { if (adC1) adC1.value = c; if (adC1Hex) adC1Hex.value = c; }
    function setC2(c) { if (adC2) adC2.value = c; if (adC2Hex) adC2Hex.value = c; }

    if (adC0) adC0.addEventListener('input', () => { if (adC0Hex) adC0Hex.value = adC0.value; syncFormToLivePreview(); });
    if (adC1) adC1.addEventListener('input', () => { if (adC1Hex) adC1Hex.value = adC1.value; syncFormToLivePreview(); });
    if (adC2) adC2.addEventListener('input', () => { if (adC2Hex) adC2Hex.value = adC2.value; syncFormToLivePreview(); });

    if (adC0Hex) adC0Hex.addEventListener('input', () => { if (/^#[0-9A-Fa-f]{6}$/.test(adC0Hex.value)) { adC0.value = adC0Hex.value; syncFormToLivePreview(); } });
    if (adC1Hex) adC1Hex.addEventListener('input', () => { if (/^#[0-9A-Fa-f]{6}$/.test(adC1Hex.value)) { adC1.value = adC1Hex.value; syncFormToLivePreview(); } });
    if (adC2Hex) adC2Hex.addEventListener('input', () => { if (/^#[0-9A-Fa-f]{6}$/.test(adC2Hex.value)) { adC2.value = adC2Hex.value; syncFormToLivePreview(); } });

    document.querySelectorAll('.palette-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        setC0(btn.dataset.c0);
        setC1(btn.dataset.c1);
        setC2(btn.dataset.c2);
        syncFormToLivePreview();
      });
    });

    if (adTitleInput) adTitleInput.addEventListener('input', syncFormToLivePreview);
    if (adDescInput) adDescInput.addEventListener('input', syncFormToLivePreview);

    function syncFormToLivePreview() {
      const type = document.querySelector('input[name="ad-type"]:checked')?.value || 'gradient';
      const liveAd = {
        title: (adTitleInput && adTitleInput.value.trim()) || 'Preview Title',
        description: (adDescInput && adDescInput.value.trim()) || 'Preview description line',
        type: type,
        gradient_c0: (adC0 && adC0.value) || '#1E3A8A',
        gradient_c1: (adC1 && adC1.value) || '#2563EB',
        gradient_c2: (adC2 && adC2.value) || '#4F46E5',
        image_url: (adImageUrlInput && adImageUrlInput.value) || ''
      };
      renderPreviewSlide(liveAd);
    }

    if (uploadImageBtn && adImageFile) {
      uploadImageBtn.addEventListener('click', () => adImageFile.click());
      adImageFile.addEventListener('change', async () => {
        const file = adImageFile.files[0];
        if (!file) return;

        if (file.size > 5 * 1024 * 1024) {
          alert('File size exceeds 5MB limit.');
          return;
        }

        const formData = new FormData();
        formData.append('file', file);

        uploadImageBtn.disabled = true;
        uploadImageBtn.textContent = 'Uploading...';

        try {
          const res = await apiRequest('/api/admin/upload-image', {
            method: 'POST',
            body: formData
          });

          const data = await parseResponseJSON(res);
          if (!res.ok) throw new Error(data.error || 'Upload failed');

          adImageUrlInput.value = data.image_url;
          imagePreviewThumb.style.backgroundImage = `url('${data.image_url}')`;
          imagePreviewThumb.textContent = '';
          syncFormToLivePreview();
          showToast('Image uploaded');
        } catch (err) {
          alert('Upload error: ' + err.message);
        } finally {
          uploadImageBtn.disabled = false;
          uploadImageBtn.textContent = 'Choose Graphic...';
        }
      });
    }

    if (adForm) {
      adForm.addEventListener('submit', async (e) => {
        e.preventDefault();
        const saveBtn = document.getElementById('modal-save-btn');
        if (saveBtn) {
          saveBtn.disabled = true;
          saveBtn.textContent = 'Saving...';
        }

        const id = adIdInput.value;
        const type = document.querySelector('input[name="ad-type"]:checked')?.value || 'gradient';
        const payload = {
          title: adTitleInput.value.trim(),
          description: adDescInput.value.trim(),
          type: type,
          gradient_c0: adC0.value,
          gradient_c1: adC1.value,
          gradient_c2: adC2.value,
          image_url: adImageUrlInput.value,
          link_url: adLinkInput.value.trim(),
          is_active: adIsActiveInput.checked
        };

        if (type === 'image' && !payload.image_url) {
          alert('Please upload a graphic for image-style ads.');
          if (saveBtn) {
            saveBtn.disabled = false;
            saveBtn.textContent = 'Save';
          }
          return;
        }

        try {
          let res;
          if (id) {
            payload.id = id;
            res = await apiRequest(`/api/admin/ads/${encodeURIComponent(id)}`, {
              method: 'PUT',
              body: payload
            });
          } else {
            res = await apiRequest('/api/admin/ads', {
              method: 'POST',
              body: payload
            });
          }

          const data = await parseResponseJSON(res);
          if (!res.ok) throw new Error(data.error || 'Failed to save ad');

          closeModal();
          showToast('Ad saved');
          await loadAds();
        } catch (err) {
          alert('Error saving ad: ' + err.message);
        } finally {
          if (saveBtn) {
            saveBtn.disabled = false;
            saveBtn.textContent = 'Save';
          }
        }
      });
    }

    // --- Initial View Load ---
    switchNavTab('overview');
  }

  function escapeHTML(str) {
    if (!str) return '';
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#039;');
  }

  // Initial session check
  document.addEventListener('DOMContentLoaded', checkSession);

})();
