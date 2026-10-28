import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import 'katex/dist/katex.min.css'
import 'highlight.js/styles/github-dark-dimmed.css'
import './App.css'
import App from './App.tsx'
import { ThemeProvider } from './contexts/ThemeContext'
import { BrowserRouter } from 'react-router-dom'
import { ProtectedRoute } from './components/ProtectedRoute'
import { TooltipProvider } from '@/components/ui/tooltip'
import { ToastProvider } from '@/components/ui/toast'
import { ChatEventsProvider } from './contexts/ChatEventsContext'
import { GlobalUploadProvider } from './contexts/GlobalUploadContext'
import { LiveSessionProvider } from './features/live/LiveSessionProvider'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { setupAuthInterceptor } from './lib/authInterceptor'

// Initialize the global fetch interceptor for auth
setupAuthInterceptor();

const queryClient = new QueryClient()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <BrowserRouter>
          <TooltipProvider>
            <ToastProvider>
              <ChatEventsProvider>
                {/* Above the auth gate and the routes: a live meeting keeps recording across navigation */}
                <LiveSessionProvider>
                  <ProtectedRoute>
                    <GlobalUploadProvider>
                      <App />
                    </GlobalUploadProvider>
                  </ProtectedRoute>
                </LiveSessionProvider>
              </ChatEventsProvider>
            </ToastProvider>
          </TooltipProvider>
        </BrowserRouter>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
