/**
 * XeroxGo Mobile Upload & Print Portal
 * Pure vanilla JavaScript implementation reproducing the reference UI flow.
 */

(function () {
  'use strict';

  // --- Configuration from DOM ---
  const container = document.querySelector('.app-container');
  if (!container) return;

  let kioskID = container.dataset.kioskId;
  const pathMatch = window.location.pathname.match(/\/k\/([^/]+)/);
  const searchParams = new URLSearchParams(window.location.search);
  const urlKiosk = (pathMatch && pathMatch[1]) || searchParams.get('kiosk_id');
  if (urlKiosk) {
    kioskID = urlKiosk;
  } else if (!kioskID) {
    kioskID = 'kiosk-demo';
  }
  const devMode = container.dataset.devMode === 'true';
  let bwSimplexPricePaise = parseInt(container.dataset.pricePerSheetPaise, 10) || 200;
  let bwDuplexPricePaise = 150;
  let colorSimplexPricePaise = 1000;
  let colorDuplexPricePaise = 750;
  let colorHardwareEnabled = false;
  let bwPricePerSheetPaise = bwSimplexPricePaise;
  let colorPricePerSheetPaise = colorSimplexPricePaise;

  // --- State Variables ---
  let file = null;
  let fileBuffer = null;
  let convertedPdfBytes = null;
  let pdfLibDoc = null;
  let pdfJsDoc = null;
  let totalDocPages = 0;
  let pageList = [];
  let pageSelection = 'All Pages';
  let customPageRange = '';
  let customRangeError = '';
  let copies = 1;
  let mode = 'B&W';
  let sides = 'Single-sided';
  let pagesPerSheet = '1';
  let userName = '';
  let activeOrderID = null;
  let pollTimer = null;
  let activeSideIndex = 0;
  let deferredPwaPrompt = null;

  // --- DOM Elements ---
  const bannerTrack = document.getElementById('banner-track');
  const bannerDots = document.getElementById('banner-dots');
  const bannerCloseBtn = document.getElementById('banner-close-btn');
  const bannerContainer = document.getElementById('promo-banner');

  const pwaInstallBanner = document.getElementById('pwa-install-banner');
  const pwaDismissBtn = document.getElementById('pwa-dismiss-btn');
  const pwaInstallBtn = document.getElementById('pwa-install-btn');
  const pwaMiniPill = document.getElementById('pwa-mini-pill');
  const pwaPillBtn = document.getElementById('pwa-pill-btn');
  const pwaGuideModal = document.getElementById('pwa-guide-modal');
  const pwaModalCloseBtn = document.getElementById('pwa-modal-close-btn');
  const pwaModalGotItBtn = document.getElementById('pwa-modal-gotit-btn');

  const uploadSection = document.getElementById('upload-section');
  const uploadArea = document.getElementById('upload-area');
  const fileInput = document.getElementById('file-input');
  const filePreviewCard = document.getElementById('file-preview-card');
  const fileNameDisplay = document.getElementById('file-name-display');
  const fileMetaDisplay = document.getElementById('file-meta-display');
  const removeFileBtn = document.getElementById('remove-file-btn');
  const uploadAlert = document.getElementById('upload-alert');

  const printOptionsSection = document.getElementById('print-options-section');
  const btnPageSelectionAll = document.getElementById('btn-page-selection-all');
  const btnPageSelectionCustom = document.getElementById('btn-page-selection-custom');
  const customRangeBox = document.getElementById('custom-range-box');
  const customRangeInput = document.getElementById('custom-range-input');
  const customRangeFeedback = document.getElementById('custom-range-feedback');

  const copiesDecBtn = document.getElementById('copies-dec');
  const copiesIncBtn = document.getElementById('copies-inc');
  const copiesValDisplay = document.getElementById('copies-val');

  const btnModeBW = document.getElementById('btn-mode-bw');
  const btnModeColor = document.getElementById('btn-mode-color');
  const btnSideSingle = document.getElementById('btn-side-single');
  const btnSideDouble = document.getElementById('btn-side-double');

  const btnNUp1 = document.getElementById('btn-nup-1');
  const btnNUp2 = document.getElementById('btn-nup-2');

  const reviewSection = document.getElementById('review-section');
  const reviewCard = document.getElementById('review-card');
  const summaryFileName = document.getElementById('summary-file-name');
  const summaryPages = document.getElementById('summary-pages');
  const summaryOptions = document.getElementById('summary-options');
  const summaryCopies = document.getElementById('summary-copies');
  const summaryPrice = document.getElementById('summary-price');
  const btnEditSettings = document.getElementById('btn-edit-settings');
  const userNameInput = document.getElementById('user-name-input');
  const btnPayUpi = document.getElementById('btn-pay-upi');
  const uploadProgressCard = document.getElementById('upload-progress-card');
  const uploadProgressBar = document.getElementById('upload-progress-bar');
  const reviewAlert = document.getElementById('review-alert');

  // Status & Tracking DOM
  const statusSection = document.getElementById('status-section');
  const statusOrderId = document.getElementById('status-order-id');
  const statusKioskName = document.getElementById('status-kiosk-name');
  const statusPickupPin = document.getElementById('status-pickup-pin');
  const statusDisplayCode = document.getElementById('status-display-code');
  const pinTimerBadge = document.getElementById('pin-timer-badge');
  const pinCountdownText = document.getElementById('pin-countdown-text');
  const btnRefreshPin = document.getElementById('btn-refresh-pin');
  const pinRefreshError = document.getElementById('pin-refresh-error');
  const statusBadge = document.getElementById('status-badge');
  const statusProgressFill = document.getElementById('status-progress-fill');
  const statusPagesText = document.getElementById('status-pages-text');
  const statusSheetsText = document.getElementById('status-sheets-text');
  const statusErrorAlert = document.getElementById('status-error-alert');
  const statusSuccessAlert = document.getElementById('status-success-alert');

  // PIN Countdown & Refresh Timer
  let pinCountdownTimer = null;

  function stopPinCountdown() {
    if (pinCountdownTimer) {
      clearInterval(pinCountdownTimer);
      pinCountdownTimer = null;
    }
  }

  function startPinCountdown(expiresAtMs) {
    stopPinCountdown();

    function tick() {
      const now = Date.now();
      const remainingSec = Math.max(0, Math.floor((expiresAtMs - now) / 1000));
      const mins = Math.floor(remainingSec / 60);
      const secs = remainingSec % 60;
      const formatted = `${String(mins).padStart(2, '0')}:${String(secs).padStart(2, '0')}`;

      if (remainingSec > 0) {
        if (pinCountdownText) pinCountdownText.textContent = `PIN valid for ${formatted}`;
        if (pinTimerBadge) {
          if (remainingSec <= 120) {
            pinTimerBadge.className = 'pin-timer-badge expiring-soon';
          } else {
            pinTimerBadge.className = 'pin-timer-badge';
          }
        }
      } else {
        if (pinCountdownText) pinCountdownText.textContent = 'PIN Expired';
        if (pinTimerBadge) pinTimerBadge.className = 'pin-timer-badge expired';
        stopPinCountdown();
      }
    }

    tick();
    pinCountdownTimer = setInterval(tick, 1000);
  }

  if (btnRefreshPin) {
    btnRefreshPin.addEventListener('click', async () => {
      if (!activeOrderID) return;
      let secret = '';
      try {
        secret = sessionStorage.getItem('xeroxgo_secret_' + activeOrderID) || '';
      } catch (_) {}

      if (!secret) {
        if (pinRefreshError) {
          pinRefreshError.textContent = 'Order ownership secret not found in session. Please upload your document again.';
          pinRefreshError.classList.remove('hidden');
        }
        return;
      }

      btnRefreshPin.disabled = true;
      if (pinRefreshError) pinRefreshError.classList.add('hidden');

      try {
        const resp = await fetch(`/api/orders/${encodeURIComponent(activeOrderID)}/pin/refresh`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({ order_secret: secret })
        });

        const resData = await resp.json().catch(() => ({}));
        if (!resp.ok) {
          throw new Error(resData.error || `Refresh failed (${resp.status})`);
        }

        const newPIN = String(resData.pickup_pin || '');
        if (statusPickupPin) statusPickupPin.textContent = newPIN;
        if (statusDisplayCode) statusDisplayCode.textContent = newPIN;

        const expiresAtMs = (resData.pin_expires_at ? resData.pin_expires_at * 1000 : (Date.now() + (resData.pin_expires_in_seconds || 900) * 1000));
        startPinCountdown(expiresAtMs);

        if (pinRefreshError) pinRefreshError.classList.add('hidden');
      } catch (err) {
        if (pinRefreshError) {
          pinRefreshError.textContent = err.message || 'Failed to refresh PIN. Please try again.';
          pinRefreshError.classList.remove('hidden');
        }
      } finally {
        btnRefreshPin.disabled = false;
      }
    });
  }

  // Live Sheet Preview DOM
  const liveSheetContainer = document.getElementById('live-sheet-container');
  const paperSheet = document.getElementById('paper-sheet');
  const livePreviewCanvas = document.getElementById('live-preview-canvas');
  const sheetLoadingSpinner = document.getElementById('sheet-loading-spinner');
  const sheetSpinnerText = document.getElementById('sheet-spinner-text');
  const sheetFallback = document.getElementById('sheet-fallback');
  const sheetFallbackTitle = document.getElementById('sheet-fallback-title');
  const sheetFallbackDesc = document.getElementById('sheet-fallback-desc');
  const previewColorBadge = document.getElementById('preview-color-badge');
  const previewSheetBadge = document.getElementById('preview-sheet-badge');
  const sheetPageIndicator = document.getElementById('sheet-page-indicator');
  const btnSheetPrev = document.getElementById('btn-sheet-prev');
  const btnSheetNext = document.getElementById('btn-sheet-next');
  const btnSheetFlip = document.getElementById('btn-sheet-flip');
  const sheetFlipText = document.getElementById('sheet-flip-text');

  // Preview State & Resource Management
  let imageElement = null;
  let imageBlobUrl = null;
  let isImageDoc = false;

  let activeRenderTask = null;
  let currentRenderGen = 0;
  const MAX_CACHED_PAGES = 3;
  const pageRenderCache = new Map();

  function clearPageCache() {
    pageRenderCache.clear();
  }

  function showFallbackPreview(title, desc) {
    if (sheetFallback) {
      sheetFallback.classList.remove('hidden');
      sheetFallback.style.display = 'flex';
      if (sheetFallbackTitle) sheetFallbackTitle.textContent = title || 'Preview Unavailable';
      if (sheetFallbackDesc) sheetFallbackDesc.textContent = desc || 'Document is ready to print';
    }
    if (livePreviewCanvas) livePreviewCanvas.style.display = 'none';
    if (sheetLoadingSpinner) sheetLoadingSpinner.classList.add('hidden');
  }

  function hideFallbackPreview() {
    if (sheetFallback) {
      sheetFallback.classList.add('hidden');
      sheetFallback.style.display = 'none';
    }
    if (livePreviewCanvas) livePreviewCanvas.style.display = 'block';
  }

  // --- Ensure PDF.js is available with absolute workerSrc ---
  async function ensurePdfjs() {
    if (window.pdfjsLib) {
      if (!window.pdfjsLib.GlobalWorkerOptions.workerSrc) {
        try {
          window.pdfjsLib.GlobalWorkerOptions.workerSrc = new URL('/vendor/pdf.worker.min.mjs', window.location.href).href;
        } catch (_) {
          window.pdfjsLib.GlobalWorkerOptions.workerSrc = '/vendor/pdf.worker.min.mjs';
        }
      }
      return window.pdfjsLib;
    }
    return new Promise((resolve) => {
      let resolved = false;
      const onReady = (e) => {
        if (resolved) return;
        resolved = true;
        window.removeEventListener('pdfjs-ready', onReady);
        resolve(e.detail || window.pdfjsLib);
      };
      window.addEventListener('pdfjs-ready', onReady);
      if (window.pdfjsLib) {
        resolved = true;
        window.removeEventListener('pdfjs-ready', onReady);
        resolve(window.pdfjsLib);
      }
      setTimeout(() => {
        if (!resolved) {
          resolved = true;
          window.removeEventListener('pdfjs-ready', onReady);
          resolve(window.pdfjsLib || null);
        }
      }, 2500);
    });
  }

  // --- Helper: Parse Custom Range String ---
  function parseCustomPages(rangeStr, maxPages) {
    if (!rangeStr || !rangeStr.trim()) return [];
    const parts = rangeStr.split(',');
    const selected = [];
    const seen = new Set();

    for (let part of parts) {
      part = part.trim();
      if (!part) continue;

      if (part.includes('-')) {
        const segs = part.split('-');
        if (segs.length !== 2) return null;
        const startStr = segs[0].trim();
        const endStr = segs[1].trim();
        if (!startStr || !endStr) return null;
        const start = parseInt(startStr, 10);
        const end = parseInt(endStr, 10);
        if (isNaN(start) || isNaN(end) || start < 1 || start > end || end > maxPages) return null;
        for (let i = start; i <= end; i++) {
          const idx = i - 1;
          if (!seen.has(idx)) {
            seen.add(idx);
            selected.push(idx);
          }
        }
      } else {
        const p = parseInt(part, 10);
        if (isNaN(p) || p < 1 || p > maxPages) return null;
        const idx = p - 1;
        if (!seen.has(idx)) {
          seen.add(idx);
          selected.push(idx);
        }
      }
    }
    return selected;
  }

  // --- Helper: Format Currency ---
  function formatRupees(paise) {
    if (typeof paise !== 'number' || isNaN(paise) || paise <= 0) return '₹0';
    const rupees = paise / 100;
    if (paise % 100 === 0) {
      return '₹' + rupees.toFixed(0);
    }
    return '₹' + rupees.toFixed(2);
  }

  // --- Quote State Tracking ---
  let pricingLoaded = false;
  let backendConfirmedPricePaise = null;
  let backendConfirmedOrderID = null;

  function invalidateQuote() {
    backendConfirmedPricePaise = null;
    backendConfirmedOrderID = null;
    activeOrderID = null;
  }

  // --- Helper: Dynamic Rate Labels ---
  function updateRateLabels() {
    const isDouble = sides === 'Double-sided';
    const activeBwRate = isDouble ? bwDuplexPricePaise : bwSimplexPricePaise;
    const activeColorRate = isDouble ? colorDuplexPricePaise : colorSimplexPricePaise;

    const bwLabel = document.getElementById('price-bw-label');
    if (bwLabel) {
      bwLabel.textContent = `₹${(activeBwRate / 100).toFixed(2)}/side`;
    }
    const colorLabel = document.getElementById('price-color-label');
    if (colorLabel) {
      colorLabel.textContent = `₹${(activeColorRate / 100).toFixed(2)}/side`;
    }
  }

  // --- Helper: Get Ordered Printable Pages ---
  function getPrintablePages() {
    if (!file || pageList.length === 0) return [];
    if (pageSelection === 'Custom Range') {
      if (!customPageRange.trim()) return pageList;
      const selectedIndices = parseCustomPages(customPageRange, totalDocPages);
      if (selectedIndices === null) return [];
      return selectedIndices.map(idx => pageList[idx]).filter(Boolean);
    }
    return pageList.filter(p => p.selected);
  }

  // --- Helper: Calculate Selected Count & Price ---
  function getSelectedPagesCount() {
    if (!file) return 0;
    return getPrintablePages().length;
  }

  function updatePriceAndReviewSummary() {
    const selectedCount = getSelectedPagesCount();
    const isDouble = sides === 'Double-sided';
    const totalPrintedPages = selectedCount * copies;
    const sheetsPerCopy = isDouble ? Math.ceil(selectedCount / 2) : selectedCount;
    const totalPhysicalSheets = sheetsPerCopy * copies;

    let activePrice;
    if (mode === 'Color' && colorHardwareEnabled) {
      activePrice = isDouble ? colorDuplexPricePaise : colorSimplexPricePaise;
    } else {
      activePrice = isDouble ? bwDuplexPricePaise : bwSimplexPricePaise;
    }
    const isPriceValid = pricingLoaded && (activePrice > 0);
    const estimatedPricePaise = isPriceValid ? (totalPrintedPages * activePrice) : 0;
    const displayPricePaise = (backendConfirmedPricePaise !== null) ? backendConfirmedPricePaise : estimatedPricePaise;

    if (summaryFileName) summaryFileName.textContent = file ? file.name : '-';
    if (summaryPages) {
      if (copies > 1) {
        summaryPages.textContent = `${selectedCount} ${selectedCount === 1 ? 'page' : 'pages'} · ${totalPrintedPages} sides`;
      } else {
        summaryPages.textContent = `${selectedCount} ${selectedCount === 1 ? 'page' : 'pages'}`;
      }
    }
    if (summaryOptions) {
      summaryOptions.textContent = `${mode} · ${isDouble ? 'Double Side' : 'Single Side'} · ${totalPhysicalSheets} ${totalPhysicalSheets === 1 ? 'physical sheet' : 'physical sheets'}`;
    }
    if (summaryCopies) summaryCopies.textContent = `${copies} ${copies === 1 ? 'copy' : 'copies'}`;
    if (summaryPrice) summaryPrice.textContent = formatRupees(displayPricePaise);

    // Requirement 13: Prevent checkout while pricing is pending, invalid, or inconsistent
    if (btnPayUpi) {
      if (!file || selectedCount === 0 || !isPriceValid || displayPricePaise <= 0) {
        btnPayUpi.disabled = true;
      } else {
        btnPayUpi.disabled = false;
      }
    }

    // Update feedback for custom range if visible
    if (pageSelection === 'Custom Range') {
      if (!customPageRange.trim()) {
        customRangeFeedback.textContent = 'Enter pages to print (e.g. 1-3, 5)';
        customRangeFeedback.style.color = 'var(--text-secondary)';
      } else {
        const parsed = parseCustomPages(customPageRange, totalDocPages);
        if (parsed === null) {
          customRangeFeedback.textContent = 'Invalid format or page number out of bounds';
          customRangeFeedback.style.color = 'var(--danger-color)';
        } else {
          customRangeFeedback.textContent = `Selected ${parsed.length} page(s)`;
          customRangeFeedback.style.color = 'var(--text-secondary)';
        }
      }
    }
  }

  // --- Promo Banner Carousel ---
  let currentSlide = 0;
  let totalSlides = 3;
  let bannerTimer = null;

  function setSlide(index) {
    if (totalSlides <= 0) return;
    currentSlide = (index + totalSlides) % totalSlides;
    if (bannerTrack) {
      bannerTrack.style.transform = `translateX(-${currentSlide * 100}%)`;
    }
    if (bannerDots) {
      const dots = bannerDots.querySelectorAll('.indicator');
      dots.forEach((dot, idx) => {
        dot.classList.toggle('active', idx === currentSlide);
      });
    }
  }

  function startBannerAutoSlide() {
    if (bannerTimer) clearInterval(bannerTimer);
    if (totalSlides > 1) {
      bannerTimer = setInterval(() => {
        setSlide(currentSlide + 1);
      }, 5000);
    }
  }

  function bindBannerDots() {
    if (bannerDots) {
      bannerDots.querySelectorAll('.indicator').forEach((dot, idx) => {
        dot.addEventListener('click', () => {
          setSlide(idx);
          startBannerAutoSlide();
        });
      });
    }
  }

  if (bannerCloseBtn && bannerContainer) {
    bannerCloseBtn.addEventListener('click', () => {
      if (bannerTimer) clearInterval(bannerTimer);
      bannerContainer.style.display = 'none';
    });
  }

  bindBannerDots();
  startBannerAutoSlide();

  // Dynamic Ad Fetching & Synchronization
  async function loadDynamicAds() {
    try {
      const res = await fetch('/api/ads');
      if (!res.ok) return;
      const data = await res.json();
      const ads = Array.isArray(data) ? data : (data.ads || []);
      if (!Array.isArray(ads) || ads.length === 0) return;

      if (!bannerTrack || !bannerDots) return;

      // Render dynamic slides
      bannerTrack.innerHTML = '';
      bannerDots.innerHTML = '';

      ads.forEach((ad, idx) => {
        const slide = document.createElement('div');
        slide.className = 'banner-slide';
        if (ad.type === 'image' && ad.image_url) {
          slide.style.background = `url('${ad.image_url}') center/cover no-repeat`;
        } else {
          const c0 = ad.gradient_c0 || '#1E3A8A';
          const c1 = ad.gradient_c1 || '#2563EB';
          const c2 = ad.gradient_c2 || '#4F46E5';
          slide.style.background = `linear-gradient(135deg, ${c0} 0%, ${c1} 50%, ${c2} 100%)`;
        }

        if (ad.link_url && (ad.link_url.startsWith('https://') || ad.link_url.startsWith('http://'))) {
          slide.style.cursor = 'pointer';
          slide.title = `Open ${ad.link_url}`;
          slide.addEventListener('click', () => {
            window.open(ad.link_url, '_blank', 'noopener,noreferrer');
          });
        }

        const titleEl = document.createElement('div');
        titleEl.className = 'banner-title';
        titleEl.textContent = ad.title || '';

        const descEl = document.createElement('div');
        descEl.className = 'banner-desc';
        descEl.textContent = ad.description || '';

        slide.appendChild(titleEl);
        slide.appendChild(descEl);
        bannerTrack.appendChild(slide);

        const dot = document.createElement('div');
        dot.className = `indicator ${idx === 0 ? 'active' : ''}`;
        bannerDots.appendChild(dot);
      });

      totalSlides = ads.length;
      currentSlide = 0;
      setSlide(0);
      bindBannerDots();
      startBannerAutoSlide();
    } catch (_) {
      // Graceful fallback to static HTML slides
    }
  }

  loadDynamicAds();

  // --- PWA Installation Banner & Guide ---
  window.addEventListener('beforeinstallprompt', (e) => {
    e.preventDefault();
    deferredPwaPrompt = e;
    if (pwaInstallBanner) pwaInstallBanner.classList.remove('hidden');
  });

  if (pwaDismissBtn && pwaInstallBanner && pwaMiniPill) {
    pwaDismissBtn.addEventListener('click', () => {
      pwaInstallBanner.classList.add('hidden');
      pwaMiniPill.classList.remove('hidden');
    });
  }

  function triggerPwaInstall() {
    if (deferredPwaPrompt) {
      deferredPwaPrompt.prompt();
      deferredPwaPrompt.userChoice.then(() => {
        deferredPwaPrompt = null;
      });
    } else {
      if (pwaGuideModal) pwaGuideModal.classList.remove('hidden');
    }
  }

  if (pwaInstallBtn) pwaInstallBtn.addEventListener('click', triggerPwaInstall);
  if (pwaPillBtn) pwaPillBtn.addEventListener('click', triggerPwaInstall);

  if (pwaModalCloseBtn && pwaGuideModal) {
    pwaModalCloseBtn.addEventListener('click', () => pwaGuideModal.classList.add('hidden'));
  }
  if (pwaModalGotItBtn && pwaGuideModal) {
    pwaModalGotItBtn.addEventListener('click', () => pwaGuideModal.classList.add('hidden'));
  }
  if (pwaGuideModal) {
    pwaGuideModal.addEventListener('click', (e) => {
      if (e.target === pwaGuideModal) pwaGuideModal.classList.add('hidden');
    });
  }

  // --- File Selection & Inspection ---
  const defaultUploadAreaHTML = uploadArea ? uploadArea.innerHTML : '';
  const officeExtensions = ['.doc', '.docx', '.ppt', '.pptx', '.xls', '.xlsx', '.odt', '.ods', '.odp', '.rtf', '.txt'];
  const imageExtensions = ['.jpg', '.jpeg', '.png', '.bmp', '.webp', '.tif', '.tiff'];

  function showError(msg) {
    if (uploadAlert) {
      uploadAlert.textContent = msg;
      uploadAlert.classList.remove('hidden');
    }
  }

  function clearError() {
    if (uploadAlert) {
      uploadAlert.textContent = '';
      uploadAlert.classList.add('hidden');
    }
  }

  async function convertImageToPdf(selectedFile) {
    if (!window.PDFLib) {
      throw new Error('PDF processing library not loaded.');
    }
    const pdfDoc = await window.PDFLib.PDFDocument.create();
    const page = pdfDoc.addPage([595.28, 841.89]); // A4 in points (72 DPI)

    const nameLower = selectedFile.name.toLowerCase();
    const isJpg = nameLower.endsWith('.jpg') || nameLower.endsWith('.jpeg') || selectedFile.type === 'image/jpeg';
    const isPng = nameLower.endsWith('.png') || selectedFile.type === 'image/png';

    let embeddedImage;
    if (isJpg) {
      const bytes = await selectedFile.arrayBuffer();
      embeddedImage = await pdfDoc.embedJpg(bytes);
    } else if (isPng) {
      const bytes = await selectedFile.arrayBuffer();
      embeddedImage = await pdfDoc.embedPng(bytes);
    } else {
      // For BMP, WEBP, etc: render to canvas and export as PNG
      const dataUrl = await new Promise((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(reader.result);
        reader.onerror = reject;
        reader.readAsDataURL(selectedFile);
      });
      const img = new Image();
      await new Promise((resolve, reject) => {
        img.onload = resolve;
        img.onerror = reject;
        img.src = dataUrl;
      });
      const canvas = document.createElement('canvas');
      canvas.width = img.width;
      canvas.height = img.height;
      const ctx = canvas.getContext('2d');
      ctx.drawImage(img, 0, 0);
      const pngDataUrl = canvas.toDataURL('image/png');
      const pngBytes = Uint8Array.from(atob(pngDataUrl.split(',')[1]), c => c.charCodeAt(0));
      embeddedImage = await pdfDoc.embedPng(pngBytes);
    }

    const margin = 36; // 0.5 inch margins
    const maxWidth = 595.28 - (margin * 2);
    const maxHeight = 841.89 - (margin * 2);
    const { width, height } = embeddedImage.scaleToFit(maxWidth, maxHeight);

    const rotationDeg = (pageList[0]?.rotation || 0) % 360;
    if (rotationDeg !== 0) {
      page.setRotation(window.PDFLib.degrees(rotationDeg));
    }

    page.drawImage(embeddedImage, {
      x: (595.28 - width) / 2,
      y: (841.89 - height) / 2,
      width,
      height,
    });

    return await pdfDoc.save();
  }

  function escapeHtml(str) {
    if (!str) return '';
    return str.replace(/[&<>'"]/g, tag => ({
      '&': '&amp;',
      '<': '&lt;',
      '>': '&gt;',
      "'": '&#39;',
      '"': '&quot;'
    }[tag] || tag));
  }

  let activeConversion = null; // { abortController, pollTimeoutId, jobId }
  let conversionGen = 0;

  function cancelActiveConversion() {
    if (activeConversion) {
      if (activeConversion.abortController) {
        try { activeConversion.abortController.abort(); } catch (_) {}
      }
      if (activeConversion.pollTimeoutId) {
        clearTimeout(activeConversion.pollTimeoutId);
      }
      activeConversion = null;
    }
  }

  async function convertDocumentSynchronously(selectedFile, signal, thisGen) {
    const titleEl = document.getElementById('conv-status-title');
    const descEl = document.getElementById('conv-status-desc');
    const fillEl = document.getElementById('conv-progress-fill');
    if (titleEl) titleEl.textContent = 'Converting document to PDF...';
    if (descEl) descEl.textContent = 'Processing with server conversion engine';
    if (fillEl) fillEl.style.width = '70%';

    const formData = new FormData();
    formData.append('file', selectedFile);

    const resp = await fetch('/api/convert', {
      method: 'POST',
      body: formData,
      signal: signal
    });

    if (!resp.ok) {
      const errText = await resp.text();
      throw new Error(errText || 'Conversion failed on server');
    }

    if (fillEl) fillEl.style.width = '95%';
    const pdfArrayBuffer = await resp.arrayBuffer();
    if (thisGen !== conversionGen) return null;
    return pdfArrayBuffer;
  }

  async function convertDocumentOnServer(selectedFile, thisGen) {
    const abortController = new AbortController();
    activeConversion = { abortController, pollTimeoutId: null, jobId: null };

    if (uploadArea) {
      uploadArea.innerHTML = `
        <div id="office-conversion-card" style="display:flex; flex-direction:column; align-items:center; justify-content:center; padding:1.5rem 1rem; width:100%; box-sizing:border-box;">
          <div style="width:36px; height:36px; border:3px solid #E2E8F0; border-top-color:#2563EB; border-radius:50%; animation:spin 0.8s linear infinite; margin-bottom:0.75rem;"></div>
          <div id="conv-status-title" style="font-weight:700; color:var(--text-primary); font-size:1rem; text-align:center;">Uploading document...</div>
          <div id="conv-status-desc" style="font-size:0.85rem; color:var(--text-secondary); margin-top:0.25rem; text-align:center;">Converting document to print-ready PDF</div>
          <div style="width:100%; max-width:240px; background:#E2E8F0; border-radius:999px; height:6px; margin-top:1rem; overflow:hidden;">
            <div id="conv-progress-fill" style="width:20%; height:100%; background:var(--primary-color); border-radius:999px; transition:width 0.4s ease;"></div>
          </div>
          <button id="btn-cancel-conversion" type="button" style="margin-top:0.9rem; background:transparent; border:none; color:var(--text-secondary); font-size:0.825rem; cursor:pointer; text-decoration:underline;">
            Cancel
          </button>
        </div>
      `;
      const cancelBtn = document.getElementById('btn-cancel-conversion');
      if (cancelBtn) {
        cancelBtn.addEventListener('click', (e) => {
          e.stopPropagation();
          cancelActiveConversion();
          resetFile();
        });
      }
    }

    const formData = new FormData();
    formData.append('file', selectedFile);

    // 1. Submit asynchronous conversion job with fallback to synchronous /api/convert
    let resp;
    try {
      resp = await fetch('/api/conversions', {
        method: 'POST',
        body: formData,
        signal: abortController.signal
      });
    } catch (fetchErr) {
      if (fetchErr.name === 'AbortError') throw fetchErr;
      console.warn('Network error on /api/conversions, falling back to /api/convert:', fetchErr);
      return await convertDocumentSynchronously(selectedFile, abortController.signal, thisGen);
    }

    if (resp.status === 404) {
      console.warn('/api/conversions returned 404, falling back to /api/convert');
      return await convertDocumentSynchronously(selectedFile, abortController.signal, thisGen);
    }

    if (!resp.ok) {
      const errText = await resp.text();
      throw new Error(errText || 'Failed to submit document for conversion');
    }
    const createData = await resp.json();
    if (thisGen !== conversionGen) return null;
    activeConversion.jobId = createData.job_id;

    // 2. Poll conversion status every 1-2s with backoff
    let pollDelay = 1000;
    const maxPollDuration = 90000; // 90 seconds
    const startTime = Date.now();
    let status = createData.status;
    let downloadUrl = null;

    while (status === 'queued' || status === 'processing') {
      if (thisGen !== conversionGen) return null;
      if (Date.now() - startTime > maxPollDuration) {
        throw new Error('Document conversion timed out after 90 seconds. Please try again.');
      }

      const titleEl = document.getElementById('conv-status-title');
      const descEl = document.getElementById('conv-status-desc');
      const fillEl = document.getElementById('conv-progress-fill');

      if (status === 'queued') {
        if (titleEl) titleEl.textContent = 'Waiting in queue...';
        if (descEl) descEl.textContent = 'Conversion worker is currently preparing';
        if (fillEl) fillEl.style.width = '45%';
      } else if (status === 'processing') {
        if (titleEl) titleEl.textContent = 'Converting document to PDF...';
        if (descEl) descEl.textContent = 'LibreOffice engine is processing pages';
        if (fillEl) fillEl.style.width = '75%';
      }

      await new Promise((resolve, reject) => {
        const timeoutId = setTimeout(resolve, pollDelay);
        activeConversion.pollTimeoutId = timeoutId;
        const abortHandler = () => {
          clearTimeout(timeoutId);
          reject(new DOMException('Aborted', 'AbortError'));
        };
        abortController.signal.addEventListener('abort', abortHandler, { once: true });
      });

      if (thisGen !== conversionGen) return null;

      const pollResp = await fetch(`/api/conversions/${encodeURIComponent(createData.job_id)}?token=${encodeURIComponent(createData.token)}`, {
        signal: abortController.signal
      });
      if (pollResp.status === 404) {
        console.warn('Poll endpoint returned 404, falling back to /api/convert');
        return await convertDocumentSynchronously(selectedFile, abortController.signal, thisGen);
      }
      if (!pollResp.ok) {
        const errText = await pollResp.text();
        throw new Error(errText || 'Failed to check conversion status');
      }
      const pollData = await pollResp.json();
      status = pollData.status;

      if (status === 'completed') {
        downloadUrl = pollData.download_url;
        break;
      } else if (status === 'failed') {
        throw new Error(pollData.error_message || 'Document conversion failed on server');
      }

      // Exponential backoff capped at 2500ms
      pollDelay = Math.min(Math.round(pollDelay * 1.25), 2500);
    }

    if (!downloadUrl) {
      throw new Error('Conversion completed but download URL was not provided');
    }

    const titleEl = document.getElementById('conv-status-title');
    const descEl = document.getElementById('conv-status-desc');
    const fillEl = document.getElementById('conv-progress-fill');
    if (titleEl) titleEl.textContent = 'Downloading converted PDF...';
    if (descEl) descEl.textContent = 'Preparing live preview';
    if (fillEl) fillEl.style.width = '95%';

    // 3. Fetch converted PDF
    const dlResp = await fetch(downloadUrl, { signal: abortController.signal });
    if (!dlResp.ok) {
      const errText = await dlResp.text();
      throw new Error(errText || 'Failed to download converted PDF');
    }
    const pdfArrayBuffer = await dlResp.arrayBuffer();
    if (thisGen !== conversionGen) return null;
    return pdfArrayBuffer;
  }

  async function handleFile(selectedFile) {
    clearError();
    if (!selectedFile) return;

    cancelActiveConversion();
    conversionGen++;
    const thisConversionGen = conversionGen;

    const nameLower = selectedFile.name.toLowerCase();
    const isPDF = nameLower.endsWith('.pdf') || selectedFile.type === 'application/pdf';
    const isOffice = officeExtensions.some(ext => nameLower.endsWith(ext));
    const isImage = imageExtensions.some(ext => nameLower.endsWith(ext)) || selectedFile.type.startsWith('image/');

    if (!isPDF && !isOffice && !isImage) {
      showError('Unsupported file format. Please upload PDF, Word (DOCX/DOC), PowerPoint (PPTX), Excel (XLSX), or Images (JPG, PNG).');
      return;
    }

    const maxSizeBytes = 25 * 1024 * 1024; // 25 MiB backend limit
    if (selectedFile.size > maxSizeBytes) {
      showError('File size exceeds the 25 MB limit.');
      return;
    }

    // Clean up previous file resources before loading new one
    if (activeRenderTask) {
      try { activeRenderTask.cancel(); } catch (_) {}
      activeRenderTask = null;
    }
    currentRenderGen++;
    clearPageCache();
    if (imageBlobUrl) {
      URL.revokeObjectURL(imageBlobUrl);
      imageBlobUrl = null;
    }
    imageElement = null;
    if (pdfJsDoc) {
      try { pdfJsDoc.destroy(); } catch (_) {}
      pdfJsDoc = null;
    }
    pdfLibDoc = null;
    hideFallbackPreview();

    try {
      file = selectedFile;
      let buffer = null;

      if (isImage) {
        // Direct Image Preview: bypass heavy PDF conversion for instant render
        isImageDoc = true;
        convertedPdfBytes = null;
        totalDocPages = 1;
        pageList = [{
          originalIndex: 0,
          rotation: 0,
          selected: true
        }];

        const imgUrl = URL.createObjectURL(selectedFile);
        imageBlobUrl = imgUrl;
        const img = new Image();
        await new Promise((resolve, reject) => {
          img.onload = resolve;
          img.onerror = () => reject(new Error('Failed to load image file.'));
          img.src = imgUrl;
        });
        imageElement = img;
      } else {
        isImageDoc = false;

        if (isPDF) {
          buffer = await selectedFile.arrayBuffer();
          convertedPdfBytes = null;
        } else if (isOffice) {
          const pdfArrayBuffer = await convertDocumentOnServer(selectedFile, thisConversionGen);
          if (thisConversionGen !== conversionGen || !pdfArrayBuffer) return;
          buffer = pdfArrayBuffer;
          convertedPdfBytes = new Uint8Array(pdfArrayBuffer);
        }

        // Parse with PDFLib
        if (!window.PDFLib) {
          throw new Error('PDF processing library not loaded.');
        }
        pdfLibDoc = await window.PDFLib.PDFDocument.load(buffer.slice(0));
        totalDocPages = pdfLibDoc.getPageCount();

        // Initialize page list
        pageList = Array.from({ length: totalDocPages }, (_, i) => ({
          originalIndex: i,
          rotation: 0,
          selected: true
        }));

        // Parse with PDF.js for preview canvas (with resilient fallback for mobile WebViews)
        const pdfjs = await ensurePdfjs();
        if (pdfjs) {
          try {
            const typedArray = new Uint8Array(buffer.slice(0));
            const loadingTask = pdfjs.getDocument({
              data: typedArray,
              cMapUrl: 'https://cdn.jsdelivr.net/npm/pdfjs-dist@5.6.205/cmaps/',
              cMapPacked: true,
            });
            pdfJsDoc = await loadingTask.promise;
          } catch (workerErr) {
            console.warn('PDF.js standard worker load error, attempting fallback:', workerErr);
            try {
              const typedArray = new Uint8Array(buffer.slice(0));
              const fallbackTask = pdfjs.getDocument({
                data: typedArray,
                disableWorker: true,
              });
              pdfJsDoc = await fallbackTask.promise;
            } catch (fallbackErr) {
              console.error('PDF.js fallback load error:', fallbackErr);
              pdfJsDoc = null;
            }
          }
        }
      }

      // Update UI
      if (uploadArea) {
        uploadArea.innerHTML = defaultUploadAreaHTML;
        uploadArea.classList.add('hidden');
      }
      if (filePreviewCard) filePreviewCard.classList.remove('hidden');
      if (fileNameDisplay) fileNameDisplay.textContent = file.name;
      if (fileMetaDisplay) {
        fileMetaDisplay.textContent = `${totalDocPages} ${totalDocPages === 1 ? 'page' : 'pages'} • ${(file.size / (1024 * 1024)).toFixed(2)} MB`;
      }

      updatePriceAndReviewSummary();
      activeSideIndex = 0;
      renderLiveSheet();
    } catch (err) {
      if (err.name === 'AbortError') {
        resetFile();
        return;
      }
      console.error('File load failed:', err);
      showError(err.message || 'Failed to process document. Please verify the file is valid.');
      if (uploadArea && isOffice) {
        uploadArea.innerHTML = `
          <div style="display:flex; flex-direction:column; align-items:center; justify-content:center; padding:1.5rem 1rem; width:100%; box-sizing:border-box;">
            <svg width="36" height="36" viewBox="0 0 24 24" fill="none" stroke="var(--danger-color)" stroke-width="2" style="margin-bottom:0.75rem;">
              <circle cx="12" cy="12" r="10"/><line x1="12" y1="8" x2="12" y2="12"/><line x1="12" y1="16" x2="12.01" y2="16"/>
            </svg>
            <div style="font-weight:700; color:var(--text-primary); font-size:1rem; text-align:center;">Conversion Failed</div>
            <div style="font-size:0.85rem; color:var(--text-secondary); margin-top:0.35rem; text-align:center; max-width:320px;">
              ${escapeHtml(err.message || 'Unable to convert this document.')}
            </div>
            <div style="display:flex; gap:0.75rem; margin-top:1rem;">
              <button id="btn-retry-conversion" type="button" class="btn btn-primary" style="padding:0.45rem 1rem; font-size:0.85rem;">
                Retry Conversion
              </button>
              <button id="btn-cancel-retry" type="button" class="btn" style="background:#E2E8F0; color:var(--text-primary); padding:0.45rem 1rem; font-size:0.85rem;">
                Choose Another File
              </button>
            </div>
          </div>
        `;
        document.getElementById('btn-retry-conversion')?.addEventListener('click', (e) => {
          e.stopPropagation();
          handleFile(selectedFile);
        });
        document.getElementById('btn-cancel-retry')?.addEventListener('click', (e) => {
          e.stopPropagation();
          resetFile();
        });
      } else {
        resetFile();
      }
    }
  }

  function resetFile() {
    cancelActiveConversion();
    if (activeRenderTask) {
      try { activeRenderTask.cancel(); } catch (_) {}
      activeRenderTask = null;
    }
    currentRenderGen++;
    clearPageCache();

    if (imageBlobUrl) {
      URL.revokeObjectURL(imageBlobUrl);
      imageBlobUrl = null;
    }
    imageElement = null;
    isImageDoc = false;

    if (pdfJsDoc) {
      try { pdfJsDoc.destroy(); } catch (_) {}
      pdfJsDoc = null;
    }
    pdfLibDoc = null;
    file = null;
    fileBuffer = null;
    convertedPdfBytes = null;
    totalDocPages = 0;
    pageList = [];
    pageSelection = 'All Pages';
    customPageRange = '';
    activeSideIndex = 0;
    copies = 1;
    invalidateQuote();
    if (copiesValDisplay) copiesValDisplay.textContent = '1';
    if (btnPageSelectionAll) btnPageSelectionAll.classList.add('active');
    if (btnPageSelectionCustom) btnPageSelectionCustom.classList.remove('active');
    if (customRangeBox) customRangeBox.classList.add('hidden');
    if (customRangeInput) customRangeInput.value = '';
    if (customRangeFeedback) customRangeFeedback.textContent = '';
    if (fileInput) fileInput.value = '';
    if (uploadSection) uploadSection.classList.remove('hidden');
    if (uploadArea) {
      uploadArea.innerHTML = defaultUploadAreaHTML;
      uploadArea.classList.remove('hidden');
    }
    if (filePreviewCard) filePreviewCard.classList.add('hidden');
    if (livePreviewCanvas) {
      const ctx = livePreviewCanvas.getContext('2d');
      if (ctx) ctx.clearRect(0, 0, livePreviewCanvas.width, livePreviewCanvas.height);
    }
    if (sheetPageIndicator) sheetPageIndicator.textContent = 'Page 1 of 1';
    if (previewSheetBadge) previewSheetBadge.textContent = 'Sheet 1 of 1';
    if (btnSheetPrev) btnSheetPrev.disabled = true;
    if (btnSheetNext) btnSheetNext.disabled = true;
    if (btnSheetFlip) btnSheetFlip.classList.add('hidden');
    hideFallbackPreview();
    updateRateLabels();
    updatePriceAndReviewSummary();
  }

  if (uploadArea) {
    uploadArea.addEventListener('click', () => fileInput && fileInput.click());
    uploadArea.addEventListener('dragover', (e) => {
      e.preventDefault();
      uploadArea.classList.add('drag-active');
    });
    uploadArea.addEventListener('dragleave', () => uploadArea.classList.remove('drag-active'));
    uploadArea.addEventListener('drop', (e) => {
      e.preventDefault();
      uploadArea.classList.remove('drag-active');
      if (e.dataTransfer.files && e.dataTransfer.files[0]) {
        handleFile(e.dataTransfer.files[0]);
      }
    });
  }

  if (fileInput) {
    fileInput.addEventListener('change', (e) => {
      if (e.target.files && e.target.files[0]) {
        handleFile(e.target.files[0]);
      }
    });
  }

  if (removeFileBtn) {
    removeFileBtn.addEventListener('click', resetFile);
  }

  // --- Print Settings Interactions ---
  if (btnPageSelectionAll) {
    btnPageSelectionAll.addEventListener('click', () => {
      pageSelection = 'All Pages';
      invalidateQuote();
      btnPageSelectionAll.classList.add('active');
      btnPageSelectionCustom.classList.remove('active');
      if (customRangeBox) customRangeBox.classList.add('hidden');
      pageList.forEach(p => p.selected = true);
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  if (btnPageSelectionCustom) {
    btnPageSelectionCustom.addEventListener('click', () => {
      pageSelection = 'Custom Range';
      invalidateQuote();
      btnPageSelectionCustom.classList.add('active');
      btnPageSelectionAll.classList.remove('active');
      if (customRangeBox) customRangeBox.classList.remove('hidden');
      applyCustomRange(customRangeInput ? customRangeInput.value : '');
    });
  }

  function applyCustomRange(rangeStr) {
    invalidateQuote();
    customPageRange = rangeStr;
    if (customRangeInput && customRangeInput !== document.activeElement) {
      customRangeInput.value = rangeStr;
    }

    if (!rangeStr.trim()) {
      pageList.forEach(p => p.selected = true);
    } else {
      const selected = parseCustomPages(rangeStr, totalDocPages);
      if (selected !== null) {
        const set = new Set(selected);
        pageList.forEach((p, idx) => {
          p.selected = set.has(idx);
        });
      }
    }
    activeSideIndex = 0;
    updatePriceAndReviewSummary();
    renderLiveSheet();
  }

  if (customRangeInput) {
    customRangeInput.addEventListener('input', (e) => {
      applyCustomRange(e.target.value);
    });
  }

  if (copiesDecBtn && copiesValDisplay) {
    copiesDecBtn.addEventListener('click', () => {
      if (copies > 1) {
        copies--;
        invalidateQuote();
        copiesValDisplay.textContent = copies;
        updatePriceAndReviewSummary();
      }
    });
  }

  if (copiesIncBtn && copiesValDisplay) {
    copiesIncBtn.addEventListener('click', () => {
      if (copies < 99) {
        copies++;
        invalidateQuote();
        copiesValDisplay.textContent = copies;
        updatePriceAndReviewSummary();
      }
    });
  }

  if (btnModeBW) {
    btnModeBW.addEventListener('click', () => {
      mode = 'B&W';
      invalidateQuote();
      btnModeBW.classList.add('active');
      if (btnModeColor) btnModeColor.classList.remove('active');
      updateRateLabels();
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  if (btnModeColor) {
    btnModeColor.addEventListener('click', () => {
      mode = 'Color';
      invalidateQuote();
      btnModeColor.classList.add('active');
      if (btnModeBW) btnModeBW.classList.remove('active');
      updateRateLabels();
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  async function fetchPricing() {
    try {
      const res = await fetch('/api/pricing');
      if (res.ok) {
        const data = await res.json();
        bwSimplexPricePaise = data.bw_simplex_price_paise || data.bw_price_per_sheet_paise || 200;
        bwDuplexPricePaise = data.bw_duplex_price_paise || 150;
        colorSimplexPricePaise = data.color_simplex_price_paise || data.color_price_per_sheet_paise || 1000;
        colorDuplexPricePaise = data.color_duplex_price_paise || 750;
        colorHardwareEnabled = (typeof data.color_enabled === 'boolean') ? data.color_enabled : true;

        bwPricePerSheetPaise = bwSimplexPricePaise;
        colorPricePerSheetPaise = colorSimplexPricePaise;
        pricingLoaded = true;

        // Hardware capability: if kiosk color printing is disabled, lock mobile UI to B&W
        if (!colorHardwareEnabled) {
          if (btnModeColor) btnModeColor.style.display = 'none';
          mode = 'B&W';
          if (btnModeBW) {
            btnModeBW.classList.add('active');
            btnModeBW.style.flex = '1';
          }
        } else {
          if (btnModeColor) btnModeColor.style.display = 'flex';
          if (btnModeBW) btnModeBW.style.flex = '';
        }

        updateRateLabels();
        updatePriceAndReviewSummary();
      } else {
        pricingLoaded = true;
        updateRateLabels();
        updatePriceAndReviewSummary();
      }
    } catch (_) {
      pricingLoaded = true;
      updateRateLabels();
      updatePriceAndReviewSummary();
    }
  }
  fetchPricing();

  if (btnSideSingle) {
    btnSideSingle.addEventListener('click', () => {
      sides = 'Single-sided';
      invalidateQuote();
      btnSideSingle.classList.add('active');
      btnSideDouble.classList.remove('active');
      activeSideIndex = 0;
      updateRateLabels();
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  if (btnSideDouble) {
    btnSideDouble.addEventListener('click', () => {
      sides = 'Double-sided';
      invalidateQuote();
      btnSideDouble.classList.add('active');
      btnSideSingle.classList.remove('active');
      activeSideIndex = 0;
      updateRateLabels();
      updatePriceAndReviewSummary();
      renderLiveSheet();
    });
  }

  // Edit Settings Button scrolls to Section 2
  if (btnEditSettings && printOptionsSection) {
    btnEditSettings.addEventListener('click', () => {
      printOptionsSection.scrollIntoView({ behavior: 'smooth' });
    });
  }

  // --- Live Print-Ready Sheet Preview Engine ---
  async function renderLiveSheet() {
    if (!livePreviewCanvas || pageList.length === 0) return;

    // Determine the list of pages selected to print in requested order
    const printablePages = getPrintablePages();
    if (printablePages.length === 0) {
      if (previewSheetBadge) previewSheetBadge.textContent = '0 pages selected';
      if (sheetPageIndicator) sheetPageIndicator.textContent = 'No pages selected';
      if (btnSheetPrev) btnSheetPrev.disabled = true;
      if (btnSheetNext) btnSheetNext.disabled = true;
      if (btnSheetFlip) btnSheetFlip.classList.add('hidden');
      showFallbackPreview('No Pages Selected', 'Please select at least one page to print');
      return;
    }

    const totalPrintable = printablePages.length;
    const isDouble = (sides === 'Double-sided');
    const totalSheets = isDouble ? Math.ceil(totalPrintable / 2) : totalPrintable;
    const totalSides = (isDouble && totalPrintable % 2 !== 0) ? totalPrintable + 1 : totalPrintable;

    if (activeSideIndex < 0) activeSideIndex = 0;
    if (activeSideIndex >= totalSides) activeSideIndex = totalSides - 1;

    // 1. Color vs B&W WYSIWYG reaction
    if (mode === 'B&W') {
      livePreviewCanvas.classList.add('preview-bw');
      livePreviewCanvas.classList.remove('preview-color');
      if (previewColorBadge) {
        previewColorBadge.className = 'sheet-badge badge-bw';
        previewColorBadge.innerHTML = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="10"/><path d="M12 2a10 10 0 0 1 0 20z" fill="currentColor"/></svg> B&amp;W Preview';
      }
    } else {
      livePreviewCanvas.classList.remove('preview-bw');
      livePreviewCanvas.classList.add('preview-color');
      if (previewColorBadge) {
        previewColorBadge.className = 'sheet-badge badge-color';
        previewColorBadge.innerHTML = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><circle cx="12" cy="12" r="10"/></svg> Color Preview';
      }
    }

    // 2. Duplex vs Simplex Sheet Calculation & Badges
    const isBlankSide = (isDouble && activeSideIndex === totalPrintable);
    const sheetNum = isDouble ? Math.floor(activeSideIndex / 2) + 1 : activeSideIndex + 1;
    const isFront = isDouble ? (activeSideIndex % 2 === 0) : true;

    if (isDouble) {
      if (isBlankSide) {
        if (previewSheetBadge) previewSheetBadge.textContent = `Sheet ${sheetNum} of ${totalSheets} (Back - Blank)`;
      } else {
        if (previewSheetBadge) previewSheetBadge.textContent = `Sheet ${sheetNum} of ${totalSheets} (${isFront ? 'Front' : 'Back'})`;
      }

      // Flip button
      if (btnSheetFlip && sheetFlipText) {
        btnSheetFlip.classList.remove('hidden');
        if (isFront) {
          if (activeSideIndex + 1 < totalPrintable) {
            sheetFlipText.textContent = `Flip to Back (Pg ${printablePages[activeSideIndex + 1].originalIndex + 1})`;
          } else {
            sheetFlipText.textContent = `Flip to Back (Blank)`;
          }
          btnSheetFlip.onclick = () => {
            activeSideIndex++;
            renderLiveSheet();
          };
        } else {
          sheetFlipText.textContent = `Flip to Front (Pg ${printablePages[activeSideIndex - 1].originalIndex + 1})`;
          btnSheetFlip.onclick = () => {
            activeSideIndex--;
            renderLiveSheet();
          };
        }
      }
    } else {
      // Simplex (Single-sided)
      if (previewSheetBadge) {
        previewSheetBadge.textContent = `Sheet ${sheetNum} of ${totalPrintable}`;
      }
      if (btnSheetFlip) {
        btnSheetFlip.classList.add('hidden');
      }
    }

    // 3. Page Stepper
    if (sheetPageIndicator) {
      if (isBlankSide) {
        sheetPageIndicator.textContent = `Blank Page (Back of Sheet ${sheetNum})`;
      } else {
        const activePg = printablePages[activeSideIndex];
        if (totalPrintable === totalDocPages) {
          sheetPageIndicator.textContent = `Page ${activePg.originalIndex + 1} of ${totalDocPages}`;
        } else {
          sheetPageIndicator.textContent = `Page ${activePg.originalIndex + 1} (${activeSideIndex + 1} of ${totalPrintable})`;
        }
      }
    }

    if (btnSheetPrev) btnSheetPrev.disabled = (activeSideIndex <= 0);
    if (btnSheetNext) btnSheetNext.disabled = (activeSideIndex >= totalSides - 1);

    // 4. Case A: Blank Back of Odd Duplex Sheet
    if (isBlankSide) {
      hideFallbackPreview();
      if (sheetLoadingSpinner) sheetLoadingSpinner.classList.add('hidden');

      const containerWidth = Math.min(340, (paperSheet && paperSheet.clientWidth) || 340);
      const dpr = Math.min(window.devicePixelRatio || 1, 2);
      const canvasW = Math.round(containerWidth * dpr);
      const canvasH = Math.round(containerWidth * Math.SQRT2 * dpr);

      livePreviewCanvas.width = canvasW;
      livePreviewCanvas.height = canvasH;
      livePreviewCanvas.style.width = '100%';
      livePreviewCanvas.style.height = 'auto';
      livePreviewCanvas.style.aspectRatio = '1 / 1.4142';
      if (paperSheet) paperSheet.style.aspectRatio = '1 / 1.4142';

      const ctx = livePreviewCanvas.getContext('2d');
      ctx.fillStyle = '#FFFFFF';
      ctx.fillRect(0, 0, canvasW, canvasH);

      // Subtle dashed border to represent paper boundary
      ctx.strokeStyle = '#E2E8F0';
      ctx.lineWidth = Math.round(2 * dpr);
      ctx.strokeRect(16 * dpr, 16 * dpr, canvasW - 32 * dpr, canvasH - 32 * dpr);

      // Blank page watermark
      ctx.fillStyle = '#94A3B8';
      ctx.font = `600 ${Math.round(14 * dpr)}px Inter, -apple-system, sans-serif`;
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText('Blank Page', canvasW / 2, canvasH / 2 - (10 * dpr));

      ctx.fillStyle = '#CBD5E1';
      ctx.font = `400 ${Math.round(11 * dpr)}px Inter, -apple-system, sans-serif`;
      ctx.fillText(`(Back of Sheet ${sheetNum})`, canvasW / 2, canvasH / 2 + (12 * dpr));
      return;
    }

    const activePageObj = printablePages[activeSideIndex];

    // 5. Case B: Direct Image Preview (Instant, zero conversion, low memory)
    if (isImageDoc && imageElement) {
      hideFallbackPreview();
      if (sheetLoadingSpinner) sheetLoadingSpinner.classList.add('hidden');

      const containerWidth = Math.min(340, (paperSheet && paperSheet.clientWidth) || 340);
      const dpr = Math.min(window.devicePixelRatio || 1, 2);
      const canvasW = Math.round(containerWidth * dpr);
      const canvasH = Math.round(containerWidth * Math.SQRT2 * dpr);

      livePreviewCanvas.width = canvasW;
      livePreviewCanvas.height = canvasH;
      livePreviewCanvas.style.width = '100%';
      livePreviewCanvas.style.height = 'auto';
      livePreviewCanvas.style.aspectRatio = '1 / 1.4142';
      if (paperSheet) paperSheet.style.aspectRatio = '1 / 1.4142';

      const ctx = livePreviewCanvas.getContext('2d');
      ctx.fillStyle = '#FFFFFF';
      ctx.fillRect(0, 0, canvasW, canvasH);

      const marginRatio = 0.06; // standard A4 printable margin
      const maxW = canvasW * (1 - 2 * marginRatio);
      const maxH = canvasH * (1 - 2 * marginRatio);

      const rotation = (activePageObj.rotation || 0) % 360;
      const isRotated90 = (rotation === 90 || rotation === 270);
      const effW = isRotated90 ? imageElement.naturalHeight || imageElement.height : imageElement.naturalWidth || imageElement.width;
      const effH = isRotated90 ? imageElement.naturalWidth || imageElement.width : imageElement.naturalHeight || imageElement.height;

      const scale = Math.min(maxW / effW, maxH / effH);
      const drawW = (imageElement.naturalWidth || imageElement.width) * scale;
      const drawH = (imageElement.naturalHeight || imageElement.height) * scale;

      ctx.save();
      ctx.translate(canvasW / 2, canvasH / 2);
      if (rotation !== 0) {
        ctx.rotate((rotation * Math.PI) / 180);
      }
      ctx.drawImage(imageElement, -drawW / 2, -drawH / 2, drawW, drawH);
      ctx.restore();
      return;
    }

    // 6. Case C: PDF Document Preview
    if (!pdfJsDoc) {
      showFallbackPreview('Preview Unavailable', `Document is valid (${totalDocPages} ${totalDocPages === 1 ? 'page' : 'pages'}) and ready to print`);
      return;
    }

    // Cancel obsolete rendering task before starting a new one
    if (activeRenderTask) {
      try { activeRenderTask.cancel(); } catch (_) {}
      activeRenderTask = null;
    }

    const renderGen = ++currentRenderGen;
    const pageKey = `${activePageObj.originalIndex}_rot${activePageObj.rotation || 0}`;

    // Check LRU page render cache
    if (pageRenderCache.has(pageKey)) {
      const cached = pageRenderCache.get(pageKey);
      livePreviewCanvas.width = cached.width;
      livePreviewCanvas.height = cached.height;
      livePreviewCanvas.style.width = '100%';
      livePreviewCanvas.style.height = 'auto';
      livePreviewCanvas.style.aspectRatio = `${cached.viewportWidth} / ${cached.viewportHeight}`;
      if (paperSheet) paperSheet.style.aspectRatio = `${cached.viewportWidth} / ${cached.viewportHeight}`;

      const ctx = livePreviewCanvas.getContext('2d');
      ctx.drawImage(cached.canvas, 0, 0);

      pageRenderCache.delete(pageKey);
      pageRenderCache.set(pageKey, cached);

      hideFallbackPreview();
      if (sheetLoadingSpinner) sheetLoadingSpinner.classList.add('hidden');
      return;
    }

    hideFallbackPreview();
    if (sheetLoadingSpinner) sheetLoadingSpinner.classList.remove('hidden');

    try {
      const pdfPage = await pdfJsDoc.getPage(activePageObj.originalIndex + 1);
      if (renderGen !== currentRenderGen) return;

      const rotation = ((pdfPage.rotate || 0) + (activePageObj.rotation || 0)) % 360;
      const baseViewport = pdfPage.getViewport({ scale: 1.0, rotation });

      const dpr = Math.min(window.devicePixelRatio || 1, 2); // Cap at 2 for mobile memory safety
      const containerWidth = Math.min(340, (paperSheet && paperSheet.clientWidth) || 340);
      const scale = (containerWidth / baseViewport.width) * dpr;
      const viewport = pdfPage.getViewport({ scale, rotation });

      livePreviewCanvas.width = Math.round(viewport.width);
      livePreviewCanvas.height = Math.round(viewport.height);
      livePreviewCanvas.style.width = '100%';
      livePreviewCanvas.style.height = 'auto';
      livePreviewCanvas.style.aspectRatio = `${viewport.width} / ${viewport.height}`;
      if (paperSheet) paperSheet.style.aspectRatio = `${viewport.width} / ${viewport.height}`;

      const ctx = livePreviewCanvas.getContext('2d');
      activeRenderTask = pdfPage.render({ canvasContext: ctx, viewport });
      await activeRenderTask.promise;
      activeRenderTask = null;

      if (renderGen !== currentRenderGen) return;

      // Cache rendered page bitmap (max 3 items in memory)
      const offscreen = document.createElement('canvas');
      offscreen.width = livePreviewCanvas.width;
      offscreen.height = livePreviewCanvas.height;
      const offCtx = offscreen.getContext('2d');
      offCtx.drawImage(livePreviewCanvas, 0, 0);

      if (pageRenderCache.size >= MAX_CACHED_PAGES) {
        const oldestKey = pageRenderCache.keys().next().value;
        pageRenderCache.delete(oldestKey);
      }
      pageRenderCache.set(pageKey, {
        canvas: offscreen,
        width: livePreviewCanvas.width,
        height: livePreviewCanvas.height,
        viewportWidth: viewport.width,
        viewportHeight: viewport.height,
      });

      hideFallbackPreview();
    } catch (err) {
      if (err && err.name === 'RenderingCancelledException') {
        return; // Expected cancellation from fast user interaction
      }
      console.warn('Live sheet render error:', err);
      showFallbackPreview('Preview Unavailable', 'Document is ready to print');
    } finally {
      if (renderGen === currentRenderGen && sheetLoadingSpinner) {
        sheetLoadingSpinner.classList.add('hidden');
      }
    }
  }

  // Stepper Event Listeners
  if (btnSheetPrev) {
    btnSheetPrev.addEventListener('click', () => {
      if (activeSideIndex > 0) {
        activeSideIndex--;
        renderLiveSheet();
      }
    });
  }

  if (btnSheetNext) {
    btnSheetNext.addEventListener('click', () => {
      const printablePages = getPrintablePages();
      const isDouble = (sides === 'Double-sided');
      const totalSides = (isDouble && printablePages.length % 2 !== 0)
        ? printablePages.length + 1
        : printablePages.length;
      if (activeSideIndex < totalSides - 1) {
        activeSideIndex++;
        renderLiveSheet();
      }
    });
  }

  // --- Document Upload Preparation ---
  async function prepareUploadDocument() {
    if (!file) return file;

    const printablePages = getPrintablePages();
    if (printablePages.length === 0) {
      throw new Error('Please select at least 1 page to print.');
    }

    const pdfName = file.name.replace(/\.[^/.]+$/, "") + ".pdf";

    if (isImageDoc) {
      if (!convertedPdfBytes) {
        if (sheetLoadingSpinner) {
          sheetLoadingSpinner.classList.remove('hidden');
          if (sheetSpinnerText) sheetSpinnerText.textContent = 'Formatting PDF for printer...';
        }
        convertedPdfBytes = await convertImageToPdf(file);
        if (sheetLoadingSpinner) sheetLoadingSpinner.classList.add('hidden');
      }
      return new File([convertedPdfBytes], pdfName, { type: 'application/pdf' });
    }

    if (convertedPdfBytes) {
      return new File([convertedPdfBytes], pdfName, { type: 'application/pdf' });
    }

    return file;
  }

  async function handleOrderSubmission() {
    if (reviewAlert) reviewAlert.classList.add('hidden');

    if (!pricingLoaded) {
      showError('Pricing is loading. Please wait a moment.');
      return;
    }

    if (!file) {
      showError('Please upload a document first.');
      return;
    }

    const selectedCount = getSelectedPagesCount();
    if (selectedCount === 0) {
      showError('Please select at least 1 page to print.');
      return;
    }

    const isDouble = (sides === 'Double-sided');
    const activePrice = (mode === 'Color' && colorHardwareEnabled)
      ? (isDouble ? colorDuplexPricePaise : colorSimplexPricePaise)
      : (isDouble ? bwDuplexPricePaise : bwSimplexPricePaise);
    if (activePrice <= 0) {
      showError('Unable to proceed: pricing is invalid or unavailable.');
      return;
    }

    if (btnPayUpi) btnPayUpi.disabled = true;
    if (uploadProgressCard) uploadProgressCard.classList.remove('hidden');

    try {
      // 1. Process custom page range / rotation if present
      const uploadDoc = await prepareUploadDocument();

      // 2. Prepare FormData
      const formData = new FormData();
      formData.append('file', uploadDoc);
      formData.append('copies', copies.toString());
      formData.append('duplex', sides === 'Double-sided' ? 'DUPLEX_LONG_EDGE' : 'SIMPLEX');
      formData.append('color_mode', mode === 'Color' ? 'COLOR' : 'BW');
      if (pageSelection === 'Custom Range' && customPageRange && customPageRange.trim()) {
        formData.append('page_range', customPageRange.trim());
      }

      // 3. Submit to server
      const uploadUrl = `/api/k/${encodeURIComponent(kioskID)}/upload`;
      const resp = await fetch(uploadUrl, {
        method: 'POST',
        body: formData
      });

      if (!resp.ok) {
        const errText = await resp.text();
        throw new Error(errText || `Upload failed (${resp.status})`);
      }

      const data = await resp.json();
      activeOrderID = data.order_id;
      backendConfirmedPricePaise = data.total_price_paise;
      backendConfirmedOrderID = data.order_id;

      // Securely store order_secret in sessionStorage only (never in URLs, logs, or persistent disk)
      if (data.order_secret) {
        try {
          sessionStorage.setItem('xeroxgo_secret_' + data.order_id, data.order_secret);
        } catch (_) {}
      }

      // Update summary price with authoritative backend price
      if (summaryPrice) {
        summaryPrice.textContent = formatRupees(data.total_price_paise);
      }

      // 4. Transition to Status Section
      if (uploadSection) uploadSection.classList.add('hidden');
      if (printOptionsSection) printOptionsSection.classList.add('hidden');
      if (reviewSection) reviewSection.classList.add('hidden');
      if (uploadArea) uploadArea.classList.add('hidden');
      if (filePreviewCard) filePreviewCard.classList.add('hidden');
      if (statusSection) statusSection.classList.remove('hidden');

      // Populate Order and Kiosk metadata
      if (statusOrderId) statusOrderId.textContent = data.order_id || '-';
      if (statusKioskName) statusKioskName.textContent = kioskID;

      // Display 4-digit pickup PIN preserving leading zeros
      const initialPIN = String(data.pickup_pin || '----');
      if (statusPickupPin) statusPickupPin.textContent = initialPIN;
      if (statusDisplayCode) statusDisplayCode.textContent = initialPIN;

      // Start 15-minute countdown timer based on server-provided expiry
      const expiresAtMs = (data.pin_expires_at ? data.pin_expires_at * 1000 : (Date.now() + (data.pin_expires_in_seconds || 900) * 1000));
      startPinCountdown(expiresAtMs);

      // Initialize status display with friendly labels
      updateStatusDisplay(data.status || 'PENDING_PAYMENT', 0, data.total_pages, 0, data.total_sheets, '');

      // Start live order status polling
      startPolling(activeOrderID);

      statusSection.scrollIntoView({ behavior: 'smooth' });
    } catch (err) {
      console.error('Order submission error:', err);
      if (reviewAlert) {
        reviewAlert.textContent = err.message || 'Submission failed. Please try again.';
        reviewAlert.classList.remove('hidden');
      } else {
        alert(err.message || 'Submission failed');
      }
    } finally {
      if (btnPayUpi) btnPayUpi.disabled = false;
      if (uploadProgressCard) uploadProgressCard.classList.add('hidden');
    }
  }

  if (btnPayUpi) {
    btnPayUpi.addEventListener('click', handleOrderSubmission);
  }

  // --- Live Status Polling & Customer-Friendly State Mapping ---
  const statusLabels = {
    'PENDING_PAYMENT': 'Pending Payment at Kiosk',
    'PAID_WAITING': 'Payment Confirmed — Queued',
    'DISPATCHED': 'Sent to Printer',
    'SPOOLING': 'Spooling Document',
    'PRINTING': 'Printing Document',
    'COMPLETED': 'Print Completed',
    'PAUSED_ERROR': 'Printer Paused / Error',
    'FAILED_REFUNDABLE': 'Print Failed (Refundable)'
  };

  function updateStatusDisplay(status, pagesPrinted, totalPages, sheetsPrinted, totalSheets, errReason) {
    const friendlyLabel = statusLabels[status] || status;
    if (statusBadge) {
      statusBadge.textContent = friendlyLabel;
      statusBadge.className = `status-pill status-${status}`;
    }

    if (totalPages > 0) {
      const pct = Math.min(100, Math.round((pagesPrinted / totalPages) * 100));
      if (statusProgressFill) statusProgressFill.style.width = pct + '%';
      if (statusPagesText) statusPagesText.textContent = `${pagesPrinted} / ${totalPages}`;
    }

    if (totalSheets > 0 && statusSheetsText) {
      statusSheetsText.textContent = `${sheetsPrinted} / ${totalSheets}`;
    }

    if (errReason && statusErrorAlert) {
      statusErrorAlert.textContent = errReason;
      statusErrorAlert.classList.remove('hidden');
    } else if (statusErrorAlert) {
      statusErrorAlert.classList.add('hidden');
    }

    // Disallow PIN refresh once order is paid or completed
    const isPaidOrBeyond = (status === 'PAID_WAITING' || status === 'DISPATCHED' || status === 'SPOOLING' || status === 'PRINTING' || status === 'COMPLETED');
    if (isPaidOrBeyond) {
      if (btnRefreshPin) {
        btnRefreshPin.disabled = true;
        btnRefreshPin.title = 'Order paid - PIN is no longer required';
      }
      if (pinTimerBadge && status !== 'COMPLETED') {
        pinTimerBadge.className = 'pin-timer-badge';
        if (pinCountdownText) pinCountdownText.textContent = 'Payment Received';
      }
      stopPinCountdown();
    }

    if (status === 'COMPLETED') {
      if (statusSuccessAlert) statusSuccessAlert.classList.remove('hidden');
      stopPinCountdown();
      if (pinTimerBadge) {
        pinTimerBadge.className = 'pin-timer-badge';
        if (pinCountdownText) pinCountdownText.textContent = 'Job Complete';
      }
    }
  }

  function startPolling(orderID) {
    if (pollTimer) clearInterval(pollTimer);
    pollTimer = setInterval(async () => {
      try {
        const resp = await fetch(`/api/orders/${encodeURIComponent(orderID)}/status`);
        if (!resp.ok) return;
        const data = await resp.json();

        updateStatusDisplay(
          data.status,
          data.pages_printed,
          data.total_pages,
          data.sheets_printed,
          data.total_sheets,
          data.error_reason
        );

        if (data.status === 'COMPLETED') {
          clearInterval(pollTimer);
        }
      } catch (_) {}
    }, 1200);
  }
})();
