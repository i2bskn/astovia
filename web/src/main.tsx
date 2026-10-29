import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import App from './App';
import { auth } from './auth';
import { authConfig } from './authConfig';
import { captureGoogleCallback } from './googleCallback';
import { captureLinkToken } from './links';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('Application root is missing');

// The token of an invitation or password reset link, and the result of a
// Google authorization, are taken out of the URL before the router reads it.
captureLinkToken(window.location, window.history);
captureGoogleCallback(window.location, window.history);
void authConfig.load();
auth.initialize();
createRoot(root).render(<StrictMode><BrowserRouter><App /></BrowserRouter></StrictMode>);
