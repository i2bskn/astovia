import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import App from './App';
import { auth } from './auth';
import { captureLinkToken } from './links';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('Application root is missing');

// The token of an invitation or password reset link is taken out of the URL
// before the router reads it.
captureLinkToken(window.location, window.history);
auth.initialize();
createRoot(root).render(<StrictMode><BrowserRouter><App /></BrowserRouter></StrictMode>);
