import { useEffect, useState } from "react";
import "./App.css";

const useDevLogin = import.meta.env.VITE_USE_DEV_LOGIN === "true";

function App() {
  const [nickname, setNickname] = useState("");
  const [contactEmail, setContactEmail] = useState("");
  const [contactEmailConsent, setContactEmailConsent] = useState(false);
  const [session, setSession] = useState(() => {
    const raw = window.localStorage.getItem("button-air-drop-session");
    return raw ? JSON.parse(raw) : null;
  });
  const [gameState, setGameState] = useState(null);
  const [message, setMessage] = useState("");
  const [pending, setPending] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [profileOpen, setProfileOpen] = useState(false);
  const [myHistory, setMyHistory] = useState(null);
  const [initialLoading, setInitialLoading] = useState(true);
  const [clickUsage, setClickUsage] = useState(null);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const accessToken = params.get("accessToken");
    const loginError = params.get("loginError");

    if (accessToken) {
      setSession({
        userId: "",
        contactEmail: params.get("contactEmail") ?? "",
        nickname: params.get("nickname") ?? "",
        accessToken,
      });
      setMessage("카카오 로그인되었습니다.");
      window.history.replaceState({}, "", "/");
    } else if (loginError) {
      setMessage("카카오 로그인을 완료하지 못했습니다.");
      window.history.replaceState({}, "", "/");
    }
  }, []);

  useEffect(() => {
    let active = true;

    function loadGameState(showError = true) {
      fetch("/api/game/state")
        .then((response) => response.json())
        .then((data) => {
          if (!active) {
            return;
          }
          setGameState(data);
        })
        .catch(() => {
          if (active && showError) {
            setMessage("게임 상태를 불러오지 못했습니다.");
          }
        })
        .finally(() => {
          if (active) {
            setInitialLoading(false);
          }
        });
    }

    loadGameState();

    function handleVisibilityChange() {
      if (document.visibilityState === "visible") {
        loadGameState(false);
      }
    }

    document.addEventListener("visibilitychange", handleVisibilityChange);

    return () => {
      active = false;
      document.removeEventListener("visibilitychange", handleVisibilityChange);
    };
  }, []);

  useEffect(() => {
    let ws;
    let reconnectTimer;
    let closedByEffect = false;
    const protocol = window.location.protocol === "https:" ? "wss:" : "ws:";

    function connect() {
      ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

      ws.onmessage = (event) => {
        const payload = JSON.parse(event.data);
        if (payload.type === "state") {
          setGameState(payload.state);
        }
      };

      ws.onerror = () => {
        ws.close();
      };

      ws.onclose = () => {
        if (closedByEffect) {
          return;
        }
        reconnectTimer = window.setTimeout(connect, 1500);
      };
    }

    connect();

    return () => {
      closedByEffect = true;
      if (reconnectTimer) {
        window.clearTimeout(reconnectTimer);
      }
      if (ws) {
        ws.close();
      }
    };
  }, []);

  useEffect(() => {
    if (session) {
      window.localStorage.setItem(
        "button-air-drop-session",
        JSON.stringify(session),
      );
      return;
    }
    window.localStorage.removeItem("button-air-drop-session");
  }, [session]);

  useEffect(() => {
    if (!session?.accessToken) {
      setClickUsage(null);
      return;
    }

    fetch("/api/me", {
      headers: {
        Authorization: `Bearer ${session.accessToken}`,
      },
    })
      .then((response) => {
        if (!response.ok) {
          throw new Error("load-me-failed");
        }
        return response.json();
      })
      .then((data) => {
        setClickUsage(data.clickUsage ?? null);
        setContactEmail(data.contactEmail ?? "");
        setContactEmailConsent(Boolean(data.contactEmailConsent));
        setSession((current) =>
          current
            ? {
                ...current,
                userId: data.userId ?? current.userId,
                contactEmail: data.contactEmail ?? current.contactEmail ?? "",
                nickname: data.nickname ?? current.nickname,
              }
            : current,
        );
      })
      .catch(() => {
        setSession(null);
      });
  }, [session?.accessToken]);

  useEffect(() => {
    if (!gameState) {
      return undefined;
    }

    const timer = window.setInterval(() => {
      setGameState((current) => {
        if (!current) {
          return current;
        }
        if (!current.leaderUserId) {
          return {
            ...current,
            remainingMs: current.initialMs ?? 1800000,
          };
        }
        return {
          ...current,
          remainingMs: Math.max(0, current.remainingMs - 100),
        };
      });
    }, 100);

    return () => window.clearInterval(timer);
  }, [gameState?.leaderUserId]);

  useEffect(() => {
    function handleKeydown(event) {
      if (event.key === "Escape") {
        setDrawerOpen(false);
        setHistoryOpen(false);
        setProfileOpen(false);
      }
    }

    window.addEventListener("keydown", handleKeydown);
    return () => window.removeEventListener("keydown", handleKeydown);
  }, []);

  useEffect(() => {
    if (!message) {
      return undefined;
    }

    const timeout = window.setTimeout(() => {
      setMessage("");
    }, 5000);

    return () => window.clearTimeout(timeout);
  }, [message]);

  const isLeader =
    session?.userId && gameState?.leaderUserId === session.userId;
  const leaderboard = gameState?.leaderboard ?? [];
  const yesterdayWinner = gameState?.yesterdayWinner ?? null;
  const rankingDateLabel = gameState?.rankingDate ?? "-";
  const overlayOpen = drawerOpen || historyOpen || profileOpen;

  function startKakaoLogin() {
    window.location.href = useDevLogin
      ? "/api/auth/dev-login"
      : "/api/auth/kakao/start";
  }

  async function clickButton() {
    if (!session?.accessToken) {
      setMessage("먼저 카카오 로그인해야 합니다.");
      startKakaoLogin();
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
        const errorData = await response.json().catch(() => null);
        if (errorData?.clickUsage) {
          setClickUsage(errorData.clickUsage);
          setMessage(
            `오늘 버튼 사용을 모두 썼습니다. ${errorData.clickUsage.used}/${errorData.clickUsage.limit}`,
          );
          return;
        }
        throw new Error("버튼 클릭 실패");
      }

      const data = await response.json();
      if (data.clickUsage) {
        setClickUsage(data.clickUsage);
      }
      if (data.state) {
        setGameState(data.state);
      }
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
      startKakaoLogin();
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

  function openProfile() {
    setNickname(session?.nickname ?? "");
    setContactEmail(contactEmail || session?.contactEmail || "");
    setProfileOpen(true);
    setDrawerOpen(false);
  }

  async function saveProfile(event) {
    event.preventDefault();
    if (!session?.accessToken) {
      return;
    }

    setPending(true);
    setMessage("");

    try {
      const response = await fetch("/api/me/profile", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${session.accessToken}`,
        },
        body: JSON.stringify({
          nickname,
          contactEmail,
          contactEmailConsent,
        }),
      });

      if (!response.ok) {
        const errorText = (await response.text()).trim();
        if (response.status === 409) {
          throw new Error("nickname-taken");
        }
        if (errorText === "invalid nickname") {
          throw new Error("invalid-nickname");
        }
        if (errorText === "nickname can only be changed once every 7 days") {
          throw new Error("nickname-change-limited");
        }
        if (
          errorText === "contact email can only be changed once every 7 days"
        ) {
          throw new Error("contact-email-change-limited");
        }
        if (errorText === "contact email consent required") {
          throw new Error("contact-email-consent-required");
        }
        throw new Error("save-failed");
      }

      const data = await response.json();
      setSession((current) =>
        current
          ? {
              ...current,
              contactEmail: data.contactEmail ?? "",
              nickname: data.nickname,
            }
          : current,
      );
      setContactEmail(data.contactEmail ?? "");
      setContactEmailConsent(Boolean(data.contactEmailConsent));
      setProfileOpen(false);
      setMessage("마이페이지가 저장되었습니다.");
    } catch (error) {
      if (error.message === "nickname-taken") {
        setMessage("이미 사용 중인 닉네임입니다.");
        return;
      }
      if (error.message === "invalid-nickname") {
        setMessage("닉네임 양식을 확인하십시오.");
        return;
      }
      if (error.message === "nickname-change-limited") {
        setMessage("닉네임은 7일에 한 번만 변경할 수 있습니다.");
        return;
      }
      if (error.message === "contact-email-change-limited") {
        setMessage("연락 이메일은 7일에 한 번만 변경할 수 있습니다.");
        return;
      }
      if (error.message === "contact-email-consent-required") {
        setMessage("이메일 저장을 위해 수집·이용 동의가 필요합니다.");
        return;
      }
      setMessage("마이페이지를 저장하지 못했습니다.");
    } finally {
      setPending(false);
    }
  }

  function logout() {
    setSession(null);
    setClickUsage(null);
    setDrawerOpen(false);
    setHistoryOpen(false);
    setProfileOpen(false);
    setMessage("로그아웃되었습니다.");
  }

  function closeOverlay() {
    setDrawerOpen(false);
    setHistoryOpen(false);
    setProfileOpen(false);
  }

  if (initialLoading && !gameState) {
    return <main className="loading-screen" />;
  }

  return (
    <main className="app-shell">
      {message ? <div className="toast-message">{message}</div> : null}

      <header className="topbar">
        <div className="brand">
          <span className="brand-tag">shared timer arena</span>
          <strong>button-air-drop</strong>
        </div>

        <div className="topbar-actions">
          {session ? (
            <button
              className="profile-button"
              onClick={() => setDrawerOpen((value) => !value)}
            >
              {session.nickname || "player"}
              <span className={`burger ${drawerOpen ? "is-open" : ""}`}>
                <span />
                <span />
                <span />
              </span>
            </button>
          ) : (
            <button className="kakao-button" onClick={startKakaoLogin}>
              카카오 로그인
            </button>
          )}
        </div>
      </header>

      <section className="layout">
        <div className="panel">
          <p className="section-title">Live Timer</p>
          <div className="timer">
            {formatClock(gameState?.remainingMs ?? 1800000)}
          </div>
          <button
            className="button-airdrop"
            disabled={
              pending ||
              !session?.accessToken ||
              (clickUsage ? clickUsage.remaining <= 0 : false)
            }
            onClick={clickButton}
          >
            {isLeader
              ? "지금은 내가 리더, 재클릭은 무시됨"
              : "버튼 누르고 현재 리더 되기"}
          </button>

          {session && clickUsage ? (
            <div className="usage-row">
              <strong>남은 횟수 {clickUsage.remaining}</strong>
            </div>
          ) : null}

          <section className="prize-card">
            <div className="prize-copy">
              <span className="prize-label">today reward</span>
              <strong>{formatKoreanDate(rankingDateLabel)}</strong>
            </div>
            <img
              className="prize-image"
              src="/prize.png"
              alt="오늘의 우승 상품"
            />
          </section>

          <div className="meta" />
        </div>

        <div className="panel">
          <p className="section-title">Today Ranking</p>
          <div className="ranks">
            {leaderboard.length === 0 ? (
              <div className="empty">아직 기록이 없습니다.</div>
            ) : (
              leaderboard.map((entry) => (
                <div
                  className={`rank-row ${entry.rank === 1 ? "is-champion" : ""}`}
                  key={`${entry.rank}-${entry.userId}-${entry.durationMs}`}
                >
                  <div className="rank-label">
                    <span className={`rank-badge rank-${entry.rank}`}>
                      {entry.rank === 1 ? "우승" : `${entry.rank}위`}
                    </span>
                    <span className="rank-name">{entry.displayName}</span>
                  </div>
                  <strong>{formatDuration(entry.durationMs)}</strong>
                </div>
              ))
            )}
          </div>

          <div className="winner-block">
            <p className="section-title">Yesterday Winner</p>
            {yesterdayWinner ? (
              <div className="rank-row">
                <span>{yesterdayWinner.displayName}</span>
                <strong>{formatDuration(yesterdayWinner.durationMs)}</strong>
              </div>
            ) : (
              <div className="empty">어제 기록이 없습니다.</div>
            )}
          </div>
        </div>
      </section>

      <div
        className={`overlay ${overlayOpen ? "is-open" : ""}`}
        onClick={closeOverlay}
      />

      <div className="modal-shell">
        <div
          className={`modal ${historyOpen ? "is-open" : ""}`}
          onClick={(event) => event.stopPropagation()}
        >
          <div className="modal-header">
            <div>
              <h2>오늘 내 기록</h2>
              <p>
                {myHistory?.rankingDate
                  ? `${myHistory.rankingDate} KST`
                  : "오늘 기록"}
              </p>
            </div>
            <button
              className="icon-button"
              onClick={() => setHistoryOpen(false)}
            >
              ×
            </button>
          </div>

          <div className="summary-grid">
            <div className="summary-row">
              <span>닉네임</span>
              <strong>{session?.nickname || myHistory?.nickname || "-"}</strong>
            </div>
            <div className="summary-row">
              <span>연락 이메일</span>
              <strong>
                {myHistory?.contactEmail || session?.contactEmail || "-"}
              </strong>
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
                <div
                  className="history-row"
                  key={`${entry.createdAt}-${index}`}
                >
                  <span>
                    {entry.currentRank ? `#${entry.currentRank} ` : ""}
                    {new Date(entry.createdAt).toLocaleTimeString("ko-KR", {
                      hour12: false,
                    })}
                  </span>
                  <strong>{formatDuration(entry.durationMs)}</strong>
                </div>
              ))
            ) : (
              <div className="empty">오늘 기록이 없습니다.</div>
            )}
          </div>
        </div>

        <div
          className={`modal ${profileOpen ? "is-open" : ""}`}
          onClick={(event) => event.stopPropagation()}
        >
          <div className="modal-header">
            <div>
              <h2>마이페이지</h2>
              <p>닉네임과 우승 안내용 이메일을 관리합니다.</p>
            </div>
            <button
              className="icon-button"
              onClick={() => setProfileOpen(false)}
            >
              ×
            </button>
          </div>

          <form className="auth-form" onSubmit={saveProfile}>
            <input
              type="text"
              value={nickname}
              onChange={(event) => setNickname(event.target.value)}
              placeholder="닉네임 2~6자"
            />
            <input
              type="email"
              value={contactEmail}
              onChange={(event) => setContactEmail(event.target.value)}
              placeholder="우승 안내 받을 이메일"
            />
            <label className="check-row">
              <input
                type="checkbox"
                checked={contactEmailConsent}
                onChange={(event) =>
                  setContactEmailConsent(event.target.checked)
                }
              />
              <span>
                우승 안내 및 상품 발송 관련 연락을 위해 이메일을 수집·이용하는
                데 동의합니다.
              </span>
            </label>
            <div className="modal-actions">
              <button
                className="primary-button"
                disabled={pending}
                type="submit"
              >
                저장
              </button>
            </div>
          </form>
        </div>
      </div>

      <aside className={`drawer ${drawerOpen ? "is-open" : ""}`}>
        <div className="drawer-header">
          <div className="drawer-user">
            <strong>{session?.nickname || "-"}</strong>
            <span>{session?.contactEmail || "연락 이메일 미등록"}</span>
          </div>
          <button className="icon-button" onClick={() => setDrawerOpen(false)}>
            ×
          </button>
        </div>

        <nav className="drawer-menu">
          <button className="drawer-item" onClick={openProfile}>
            마이페이지
            <span className="drawer-arrow">→</span>
          </button>
          <button className="drawer-item" onClick={openMyHistory}>
            오늘 내 기록
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
  return `${String(minutes).padStart(2, "0")}:${String(seconds).padStart(
    2,
    "0",
  )}.${String(centiseconds).padStart(2, "0")}`;
}

function formatDuration(ms) {
  const safe = Math.max(0, ms);
  const minutes = Math.floor(safe / 60000);
  const seconds = Math.floor((safe % 60000) / 1000);
  const centiseconds = Math.floor((safe % 1000) / 10);
  return `${minutes}분 ${String(seconds).padStart(2, "0")}.${String(
    centiseconds,
  ).padStart(2, "0")}초`;
}

function formatKoreanDate(dateString) {
  if (!dateString || dateString === "-") {
    return "오늘";
  }

  const [year, month, day] = dateString.split("-");
  if (!year || !month || !day) {
    return dateString;
  }

  return `${year}.${month}.${day}`;
}

export default App;
