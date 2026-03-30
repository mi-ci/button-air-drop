import { useEffect, useMemo, useState } from "react";
import "./App.css";

function App() {
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [session, setSession] = useState(() => {
    const raw = window.localStorage.getItem("button-air-drop-session");
    return raw ? JSON.parse(raw) : null;
  });
  const [gameState, setGameState] = useState(null);
  const [message, setMessage] = useState("");
  const [pending, setPending] = useState(false);
  const [loginOpen, setLoginOpen] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [myHistory, setMyHistory] = useState(null);

  useEffect(() => {
    fetch("/api/game/state")
      .then((response) => response.json())
      .then(setGameState)
      .catch(() => setMessage("게임 상태를 불러오지 못했습니다."));
  }, []);

  useEffect(() => {
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";
    const ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

    ws.onmessage = (event) => {
      const payload = JSON.parse(event.data);
      if (payload.type === "state") {
        setGameState(payload.state);
      }
    };

    return () => ws.close();
  }, []);

  useEffect(() => {
    if (session) {
      window.localStorage.setItem("button-air-drop-session", JSON.stringify(session));
      return;
    }
    window.localStorage.removeItem("button-air-drop-session");
  }, [session]);

  useEffect(() => {
    if (!gameState) {
      return undefined;
    }

    const timer = window.setInterval(() => {
      setGameState((current) => {
        if (!current) {
          return current;
        }
        return {
          ...current,
          remainingMs: Math.max(0, current.remainingMs - 100),
          heldMs: current.leaderEmail ? current.heldMs + 100 : 0,
        };
      });
    }, 100);

    return () => window.clearInterval(timer);
  }, [gameState?.leaderEmail]);

  useEffect(() => {
    function handleKeydown(event) {
      if (event.key === "Escape") {
        setLoginOpen(false);
        setDrawerOpen(false);
        setHistoryOpen(false);
      }
    }

    window.addEventListener("keydown", handleKeydown);
    return () => window.removeEventListener("keydown", handleKeydown);
  }, []);

  const isLeader = session?.email && gameState?.leaderEmail === session.email;
  const leaderboard = gameState?.leaderboard ?? [];
  const rankingDateLabel = useMemo(() => {
    if (!gameState?.rankingDate) {
      return "-";
    }
    return `${gameState.rankingDate} KST`;
  }, [gameState?.rankingDate]);
  const overlayOpen = loginOpen || drawerOpen || historyOpen;

  async function requestCode(event) {
    event.preventDefault();
    setPending(true);
    setMessage("");

    try {
      const response = await fetch("/api/auth/request", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email }),
      });

      if (!response.ok) {
        throw new Error("인증코드 요청 실패");
      }

      const data = await response.json();
      setMessage(`개발용 인증코드: ${data.devCode}`);
    } catch {
      setMessage("인증코드를 요청하지 못했습니다.");
    } finally {
      setPending(false);
    }
  }

  async function verifyCode(event) {
    event.preventDefault();
    setPending(true);
    setMessage("");

    try {
      const response = await fetch("/api/auth/verify", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, code }),
      });

      if (!response.ok) {
        throw new Error("인증 실패");
      }

      const data = await response.json();
      setSession({
        email: data.email,
        maskedEmail: data.maskedEmail,
        accessToken: data.accessToken,
      });
      setLoginOpen(false);
      setCode("");
      setMessage("로그인되었습니다.");
    } catch {
      setMessage("인증코드가 올바르지 않거나 만료되었습니다.");
    } finally {
      setPending(false);
    }
  }

  async function clickButton() {
    if (!session?.accessToken) {
      setLoginOpen(true);
      setMessage("먼저 로그인해야 합니다.");
      return;
    }

    setPending(true);
    setMessage("");

    try {
      const response = await fetch("/api/game/click", {
        method: "POST",
        headers: {
          Authorization: `Bearer ${session.accessToken}`,
        },
      });

      if (!response.ok) {
        throw new Error("버튼 클릭 실패");
      }

      const data = await response.json();
      setGameState(data);
      if (isLeader) {
        setMessage("현재 리더가 다시 누른 경우는 무시됩니다.");
      }
    } catch {
      setMessage("버튼 처리에 실패했습니다.");
    } finally {
      setPending(false);
    }
  }

  async function openMyHistory() {
    if (!session?.accessToken) {
      setLoginOpen(true);
      return;
    }

    setPending(true);
    setMessage("");

    try {
      const response = await fetch("/api/rankings/me", {
        headers: {
          Authorization: `Bearer ${session.accessToken}`,
        },
      });
      if (!response.ok) {
        throw new Error("내 기록 조회 실패");
      }
      const data = await response.json();
      setMyHistory(data);
      setHistoryOpen(true);
      setDrawerOpen(false);
    } catch {
      setMessage("오늘 내 기록을 불러오지 못했습니다.");
    } finally {
      setPending(false);
    }
  }

  function logout() {
    setSession(null);
    setDrawerOpen(false);
    setHistoryOpen(false);
    setMessage("로그아웃되었습니다.");
  }

  function closeOverlay() {
    setLoginOpen(false);
    setDrawerOpen(false);
    setHistoryOpen(false);
  }

  return (
    <main className="app-shell">
      <header className="topbar">
        <div className="brand">
          <span className="brand-tag">shared timer arena</span>
          <strong>button-air-drop</strong>
        </div>

        <div className="topbar-actions">
          {session ? (
            <button className="profile-button" onClick={() => setDrawerOpen((value) => !value)}>
              {session.maskedEmail}
              <span className={`burger ${drawerOpen ? "is-open" : ""}`}>
                <span />
                <span />
                <span />
              </span>
            </button>
          ) : (
            <button className="login-button" onClick={() => setLoginOpen(true)}>
              로그인
            </button>
          )}
        </div>
      </header>

      <section className="hero">
        <span className="pill">오늘 자정 KST 초기화</span>
        <h1>같은 타이머를 보고, 누가 가장 오래 살아남는지 보는 버튼 게임.</h1>
        <p>
          로그인 후 버튼을 누르면 현재 리더가 되고, 다른 유저가 가져가거나 타이머가 끝날 때까지
          버틴 시간이 오늘 랭킹에 기록됩니다. 현재 리더가 자기 자신 버튼을 다시 눌러도 무시됩니다.
        </p>
      </section>

      <section className="layout">
        <div className="panel">
          <p className="section-title">Live Timer</p>
          <div className="timer">{formatClock(gameState?.remainingMs ?? 600000)}</div>
          <button
            className="button-airdrop"
            disabled={pending || !session?.accessToken}
            onClick={clickButton}
          >
            {isLeader ? "지금은 내가 리더, 재클릭은 무시됨" : "버튼 누르고 현재 리더 되기"}
          </button>

          <div className="meta">
            <div className="meta-row">
              <span>현재 리더</span>
              <strong>
                {session?.email && gameState?.leaderEmail === session.email
                  ? session.email
                  : gameState?.leaderMasked || "아직 없음"}
              </strong>
            </div>
            <div className="meta-row">
              <span>현재 버틴 시간</span>
              <strong>{formatDuration(gameState?.heldMs ?? 0)}</strong>
            </div>
            <div className="meta-row">
              <span>오늘 랭킹 기준일</span>
              <strong>{rankingDateLabel}</strong>
            </div>
            <div className="meta-row">
              <span>내 상태</span>
              <strong>{session?.maskedEmail || "비로그인"}</strong>
            </div>
          </div>

          {message ? <div className="message">{message}</div> : null}
        </div>

        <div className="panel">
          <p className="section-title">Today Ranking</p>
          <div className="ranks">
            {leaderboard.length === 0 ? (
              <div className="empty">아직 기록이 없습니다.</div>
            ) : (
              leaderboard.map((entry) => (
                <div className="rank-row" key={`${entry.rank}-${entry.maskedEmail}-${entry.durationMs}`}>
                  <span>
                    #{entry.rank} {entry.email === session?.email ? session.email : entry.maskedEmail}
                  </span>
                  <strong>{formatDuration(entry.durationMs)}</strong>
                </div>
              ))
            )}
          </div>
        </div>
      </section>

      <div className={`overlay ${overlayOpen ? "is-open" : ""}`} onClick={closeOverlay} />

      <div className="modal-shell">
        <div className={`modal ${loginOpen ? "is-open" : ""}`} onClick={(event) => event.stopPropagation()}>
          <div className="modal-header">
            <div>
              <h2>이메일 로그인</h2>
              <p>비밀번호 없이 이메일 인증코드로 로그인합니다.</p>
            </div>
            <button className="icon-button" onClick={() => setLoginOpen(false)}>
              ×
            </button>
          </div>

          <form className="auth-form">
            <input
              type="email"
              value={email}
              onChange={(event) => setEmail(event.target.value)}
              placeholder="email@example.com"
            />
            <input
              type="text"
              value={code}
              onChange={(event) => setCode(event.target.value)}
              placeholder="인증코드 6자리"
            />
            <div className="modal-actions">
              <button className="primary-button" disabled={pending} onClick={requestCode}>
                인증코드 요청
              </button>
              <button className="secondary-button" disabled={pending} onClick={verifyCode}>
                코드 확인 후 로그인
              </button>
            </div>
          </form>
        </div>

        <div className={`modal ${historyOpen ? "is-open" : ""}`} onClick={(event) => event.stopPropagation()}>
          <div className="modal-header">
            <div>
              <h2>오늘 내 기록</h2>
              <p>{myHistory?.rankingDate ? `${myHistory.rankingDate} KST` : "오늘 기록"}</p>
            </div>
            <button className="icon-button" onClick={() => setHistoryOpen(false)}>
              ×
            </button>
          </div>

          <div className="summary-grid">
            <div className="summary-row">
              <span>이메일</span>
              <strong>{session?.email || myHistory?.maskedEmail || "-"}</strong>
            </div>
            <div className="summary-row">
              <span>시도 횟수</span>
              <strong>{myHistory?.attemptCount ?? 0}</strong>
            </div>
            <div className="summary-row">
              <span>최고 기록</span>
              <strong>{formatDuration(myHistory?.bestMs ?? 0)}</strong>
            </div>
          </div>

          <div className="history-list" style={{ marginTop: 16 }}>
            {myHistory?.entries?.length ? (
              myHistory.entries.map((entry, index) => (
                <div className="history-row" key={`${entry.createdAt}-${index}`}>
                  <span>{new Date(entry.createdAt).toLocaleTimeString("ko-KR", { hour12: false })}</span>
                  <strong>{formatDuration(entry.durationMs)}</strong>
                </div>
              ))
            ) : (
              <div className="empty">오늘 기록이 없습니다.</div>
            )}
          </div>
        </div>
      </div>

      <aside className={`drawer ${drawerOpen ? "is-open" : ""}`}>
        <div className="drawer-header">
          <div className="drawer-user">
            <strong>{session?.maskedEmail || "-"}</strong>
            <span>오늘 버튼 게임 메뉴</span>
          </div>
          <button className="icon-button" onClick={() => setDrawerOpen(false)}>
            ×
          </button>
        </div>

        <nav className="drawer-menu">
          <button className="drawer-item" onClick={openMyHistory}>
            오늘 내 기록
            <span className="drawer-arrow">→</span>
          </button>
          <button className="drawer-item" onClick={() => window.scrollTo({ top: 0, behavior: "smooth" })}>
            상단으로 이동
            <span className="drawer-arrow">→</span>
          </button>
          <button className="drawer-item" onClick={logout}>
            로그아웃
            <span className="drawer-arrow">→</span>
          </button>
        </nav>
      </aside>
    </main>
  );
}

function formatClock(ms) {
  const safe = Math.max(0, ms);
  const minutes = Math.floor(safe / 60000);
  const seconds = Math.floor((safe % 60000) / 1000);
  const centiseconds = Math.floor((safe % 1000) / 10);
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}.${String(
    centiseconds,
  ).padStart(2, "0")}`;
}

function formatDuration(ms) {
  const safe = Math.max(0, ms);
  const minutes = Math.floor(safe / 60000);
  const seconds = Math.floor((safe % 60000) / 1000);
  const centiseconds = Math.floor((safe % 1000) / 10);
  return `${minutes}분 ${String(seconds).padStart(2, "0")}.${String(centiseconds).padStart(
    2,
    "0",
  )}초`;
}

export default App;
