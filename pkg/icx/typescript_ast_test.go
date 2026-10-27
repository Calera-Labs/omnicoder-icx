// SPDX-License-Identifier: Apache-2.0
package icx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

func TestTypeScriptAST_PolyglotIngestionAndWalk(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. Ingest TypeScript Types & Component file
	typesTSX := `
import React from 'react';

export interface BaseProps {
    id: string;
}

export interface ButtonProps extends BaseProps {
    label: string;
    onClick: () => void;
}

export type ThemeMode = 'dark' | 'light' | 'auto';

export enum ButtonVariant {
    Primary = 'primary',
    Secondary = 'secondary',
}

export const Button: React.FC<ButtonProps> = ({ label, onClick }) => {
    return <button onClick={onClick}>{label}</button>;
};

export class NotificationManager implements BaseProps {
    id: string = 'notify-1';

    public sendAlert(msg: string): void {
        console.log(msg);
    }
}
`

	err := lat.IngestPolyglotAST("src/components/Button.tsx", []byte(typesTSX))
	if err != nil {
		t.Fatalf("IngestPolyglotAST on Button.tsx failed: %v", err)
	}

	// 2. Ingest consumer TSX file referencing Button
	appTSX := `
import React from 'react';
import { Button } from './components/Button';

export function App() {
    const handleClick = () => {
        console.log('clicked');
    };

    return (
        <div className="container">
            <Button label="Click Me" onClick={handleClick} />
        </div>
    );
}
`
	err = lat.IngestPolyglotAST("src/App.tsx", []byte(appTSX))
	if err != nil {
		t.Fatalf("IngestPolyglotAST on App.tsx failed: %v", err)
	}

	// 3. Verify QueryCodebase finds TypeScript interfaces, types, enums, components
	queries := []string{"Button", "ButtonProps", "ThemeMode", "ButtonVariant", "NotificationManager"}
	for _, q := range queries {
		qRes, err := lat.QueryCodebase(ctx, icx.QueryCodebaseRequest{
			Query:     q,
			Dimension: "3D_VOLUMETRIC",
			TopK:      5,
		})
		if err != nil {
			t.Fatalf("QueryCodebase(%q) failed: %v", q, err)
		}
		if qRes.Count == 0 {
			t.Errorf("QueryCodebase(%q) expected matches, got 0", q)
		}
	}

	// 4. Verify MultihopWalk starting from symbol "Button" traverses to App and returns ribbon
	walkRes, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "Button",
		MaxHops:   5,
		Format:    "ribbon",
	})
	if err != nil {
		t.Fatalf("MultihopWalk from Button failed: %v", err)
	}
	if !walkRes.OK {
		t.Fatalf("MultihopWalk expected OK=true, got %+v", walkRes)
	}
	if len(walkRes.Steps) == 0 {
		t.Fatalf("expected non-empty walk steps from Button")
	}

	foundApp := false
	for _, step := range walkRes.Steps {
		if strings.Contains(step.Name, "App") || strings.Contains(step.FilePath, "App.tsx") {
			foundApp = true
			break
		}
	}
	if !foundApp {
		t.Errorf("expected walk from Button to reach App, steps: %+v", walkRes.Steps)
	}

	if walkRes.Ribbon == "" || !strings.Contains(walkRes.Ribbon, "<nav:ribbon:") {
		t.Errorf("expected valid ribbon format, got: %q", walkRes.Ribbon)
	}
}
