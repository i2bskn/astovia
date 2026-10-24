import { useEffect, useState } from 'react';

// useNow returns the current time, refreshed every minute so that relative
// labels ("3分前", "今日") stay correct while the page is open.
export function useNow(): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 60_000);
    return () => window.clearInterval(timer);
  }, []);
  return now;
}
