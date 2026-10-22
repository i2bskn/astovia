import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { auth } from './auth';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('Application root is missing');

auth.initialize();
createRoot(root).render(<StrictMode><App /></StrictMode>);
