import { useAuthStore } from '../features/auth/store/authStore';
import { refreshToken, navigateToHome, parseRequestUrl } from './authHelpers';
import './authTypes';

// Refresh tokens rotate, so concurrent 401s (e.g. live chunk uploads and state polls
// firing together) must share one refresh; a second refresh with the old token fails
// and would log the user out mid-meeting.
let refreshInFlight: Promise<string | null> | null = null;

function refreshTokenOnce(): Promise<string | null> {
    if (!refreshInFlight) {
        refreshInFlight = refreshToken().finally(() => {
            refreshInFlight = null;
        });
    }
    return refreshInFlight;
}

function bearerToken(init: RequestInit): string | null {
    const value = new Headers(init.headers).get('Authorization');
    return value?.startsWith('Bearer ') ? value.slice(7) : null;
}

export function setupAuthInterceptor(): void {
    if (window.__scriberr_original_fetch) {
        return;
    }

    const originalFetch = window.fetch.bind(window);
    window.__scriberr_original_fetch = originalFetch;

    const wrappedFetch: typeof window.fetch = async (input, init) => {
        const url = parseRequestUrl(input);
        const isAuthEndpoint = url.includes('/api/v1/auth/');

        const state = useAuthStore.getState();
        const token = state.token;

        let requestInit = init || {};
        if (token && !isAuthEndpoint) {
            const headers = new Headers(requestInit.headers);
            if (!headers.has('Authorization')) {
                headers.set('Authorization', `Bearer ${token}`);
                requestInit = { ...requestInit, headers };
            }
        }

        let response = await originalFetch(input, requestInit);

        if (response.status === 401 && !isAuthEndpoint) {
            // If another request already refreshed since this one was sent, just retry
            // with the current token instead of rotating again.
            const current = useAuthStore.getState().token;
            const sent = bearerToken(requestInit);
            const newToken = current && sent && current !== sent ? current : await refreshTokenOnce();

            if (newToken) {
                const retryHeaders = new Headers(requestInit.headers);
                retryHeaders.set('Authorization', `Bearer ${newToken}`);
                const retryInit = { ...requestInit, headers: retryHeaders };

                response = await originalFetch(input, retryInit);
                if (response.status !== 401) return response;
            }

            state.logout();
            navigateToHome();
        }

        return response;
    };

    window.fetch = wrappedFetch;
}
