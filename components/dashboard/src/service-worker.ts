/**
 * Copyright (c) 2021 Gitpod GmbH. All rights reserved.
 * Licensed under the GNU Affero General Public License (AGPL).
 * See License-AGPL.txt in the project root for license information.
 */

/// <reference lib="webworker" />

declare const self: ServiceWorkerGlobalScope;

// to suppress workbox compilation errros
let ignored = (self as any).__WB_MANIFEST;
ignored = undefined;

self.addEventListener('fetch', () => {
    // pass through just to enable PWA, see https://web.dev/install-criteria/#criteria
    // we already agressively cache everything by leveraging browser caching
});

export default null;