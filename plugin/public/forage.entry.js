// forage.entry.js — runs inside Stash's main SPA. Adds a "Forage" nav
// button that opens the launcher (launch.html), which redirects to the
// standalone forage app served by the forager daemon. forage no longer
// ships its full UI inside Stash — the daemon serves it at /.
//
// Same DOM-injection pattern as binge (a real <a href> rather than
// PluginApi.patch.instead so other plugins' nav scanners can discover it).
(function () {
    'use strict';

    if (window.forageNavLoaded) return;
    window.forageNavLoaded = true;

    var FORAGE_PATH = '/plugin/forage/assets/launch.html';
    // Acorn glyph — two-path composite (cap + nut), same SVG the
    // in-app header uses (App.tsx AcornIcon). currentColor lets Stash's
    // nav inherit text colour for the icon.
    var ACORN_PATH_1 = "M68.173,182.354c-98.206,98.221-73.314,248.837-30.337,291.827c43.005,43.006,193.634,67.898,291.84-30.323c51.09-51.075,90.54-90.525,90.54-90.525L158.711,91.83C158.711,91.83,119.263,131.279,68.173,182.354z M194.604,412.66c22.309-4.264,44.728-14.015,63.814-33.059c8.334-8.334,21.835-8.334,30.17,0c8.334,8.334,8.334,21.837,0,30.17c-25.949,25.99-57.035,39.352-86.039,44.826c-29.114,5.472-56.284,3.472-77.259-1.417c-11.474-2.71-18.586-14.183-15.891-25.656c2.696-11.474,14.196-18.586,25.67-15.891C150.404,415.272,172.379,416.924,194.604,412.66z";
    var ACORN_PATH_2 = "M456.595,96.803l22.837-22.851c10.194-10.196,10.194-26.697,0-36.878c-10.169-10.183-26.698-10.196-36.866,0l-22.35,22.336C336.065-12.697,235.721-15.1,190.27,30.35c-20.057,20.044-22.835,22.837-23.141,23.142l291.41,291.41c0.306-0.306,3.084-3.084,23.142-23.142C526.242,277.2,524.631,179.909,456.595,96.803z";

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
                '<path fill="currentColor" d="' + ACORN_PATH_1 + '"/>' +
                '<path fill="currentColor" d="' + ACORN_PATH_2 + '"/>' +
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
