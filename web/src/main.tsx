import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router';
import App from './App';
import { auth } from './auth';
import { basename } from './view';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('Application root is missing');

auth.initialize();
createRoot(root).render(<StrictMode><BrowserRouter basename={basename}><App /></BrowserRouter></StrictMode>);
