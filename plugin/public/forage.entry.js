// forage.entry.js — runs inside Stash's main SPA. Adds a "Forage" nav
// button that opens the SPA fullscreen at /plugin/forage/assets/index.html.
//
// Same DOM-injection pattern as binge (a real <a href> rather than
// PluginApi.patch.instead so other plugins' nav scanners can discover it).
(function () {
    'use strict';

    if (window.forageNavLoaded) return;
    window.forageNavLoaded = true;

    var FORAGE_PATH = '/plugin/forage/assets/index.html';
    // Sprout icon (mushroom + leaves) — vaguely forage-y.
    var ICON_PATH = "M256 32C150 32 64 118 64 224c0 80 64 144 64 224h256c0-80 64-144 64-224 0-106-86-192-192-192zm0 80c66 0 120 54 120 120 0 33-13 63-35 84-22-21-52-34-85-34s-63 13-85 34c-22-21-35-51-35-84 0-66 54-120 120-120z";

    function addNavButton() {
        if (document.getElementById('forage-nav-container')) return;

        var scenesLink = document.querySelector('a[href="/scenes"]');
        var navbar = document.querySelector('nav .navbar-nav');
        if (!navbar || !scenesLink) return;

        var navContainer = document.createElement('div');
        navContainer.id = 'forage-nav-container';
        navContainer.className = scenesLink.parentElement.className;

        var navLink = document.createElement('a');
        navLink.href = FORAGE_PATH;
        navLink.target = '_blank';
        navLink.rel = 'noopener noreferrer';
        navLink.id = 'forage-nav-button';
        navLink.title = 'Forage';
        navLink.setAttribute('aria-label', 'Forage');
        navLink.className = scenesLink.className.replace(/\bactive\b/g, '').trim();

        navLink.innerHTML =
            '<svg aria-hidden="true" focusable="false" class="svg-inline--fa fa-icon nav-menu-icon d-block d-xl-inline mb-2 mb-xl-0" role="img" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512">' +
                '<path fill="currentColor" d="' + ICON_PATH + '"/>' +
            '</svg>' +
            '<span>Forage</span>';

        navContainer.appendChild(navLink);
        navbar.appendChild(navContainer);
    }

    if (typeof PluginApi !== 'undefined' && PluginApi && PluginApi.Event && PluginApi.Event.addEventListener) {
        PluginApi.Event.addEventListener('stash:location', function () {
            addNavButton();
        });
    }

    addNavButton();
    if (!document.getElementById('forage-nav-container')) {
        var observer = new MutationObserver(function () {
            addNavButton();
            if (document.getElementById('forage-nav-container')) {
                observer.disconnect();
            }
        });
        observer.observe(document.body, { childList: true, subtree: true });
    }
})();
